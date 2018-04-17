
# README for grep-streams

grep-streams is a scalable, fault tolerant and highly available,
exactly once, stream processing version of grep(1).

grep-streams is a service which provides a way to search and subscribe
to sequenced stream results in kafka, without being limited to reading
all messages in a topic.  It sends the filter to the data, caches
popular searches, and can resume from reconnects quickly.

The MVP goal for this is:

* SSE subscribers

* All kafka topics are JSON data

* All kafka keys are string arrays

* MongoDB query AST syntax

* Full scalability and reliability

## Use cases

* return a stream of events, matching:

- topic string array or regex ('filter')

- data query AST

* stream should report stable "event IDs" and allow reconnect with
  exactly delivery of messages (requires caller to de-duplicate events
  with identical event IDs)

## Implementation plan

The full scalable plan calls for four logical workers, each of which
are distributed over the cluster and can run on the same nodes as
kafka.

Some of them have dependent workers that are just RocksDB/sstable
indexers so that other components can look up keys efficiently.  These
are marked with (+).

* sse-grep: - handles socket IO and maps greps to topic+partitions
 * <= `sse_out_grep`
 * => `sse_in_grep`(c+) - PartKey/Prefix: <topic:partid:filterid>
 * => `grep_filters`(c+) - <topic:filterid>
 * => `sse_ack_grep`(+) - PartKey: <topic:filterid>
 * => `grep_heartbeat`
* grep-streams - embarrassingly parallel component
 * <= (all data topics) ('PK' of topic:partid)
 * <= `sse_in_grep`(ss)
 * <= `grep_filters`(ss)
 * => `grep_match`(<c+) - PartKey: <topic:filterid>
* grep stream sequencer - indexes/reduces grep results
 * <= `grep_match` (PK)
 * => `grep_match_seq`(+) - PartKey: <topic:filterid>
* grep cursor - implements scrollable `grep_match_seq` cursors
* <= `sse_in_grep` (g)
 * <= `sse_ack_grep` (PK+CP,ss)
 * <= `grep_match_seq` (PK+CP,ss)
 * => `sse_out_grep` - PartKey: <pod>
* grep updog
 * <= `grep_heartbeat`
 * => `sse_in_grep` (c)

output topic flags:
(c) - compact
(+) - has sstable index topic
(<c+) - has sstable index topic, also sstable read

input topic flags:
(g) - global sstable
(PK) - partition key of worker
(PK+CP) - copartitioned consumption
(ss) - partitioned sorted string table (looked up from end)

# sse-grep ("map")

This is an incoming HTTP service.  It gets the request, sends network
events to topics and consumes messages from its outbound topic.  Its
pod name is used for routing outbound messages, so a given pod only
needs to consume a single partition of `sse_out_grep`.

Produces (with ~1ms-20ms Nagle flush):

* 'sse_in_grep' - map from [topic:partid:<pod:socketid>:filter] to [epoch:startid:jsonql:windowsize]
 - disconnect/unsubscribe - tombstone

  log.cleanup.policy=compact
  min.compaction.lag.ms - 60000 or so

* 'sse_ack_grep' - log of [topic:filter:jsonql:<pod:socketid>] value [time:eventid].  Repeats for session idle keepalives.

* 'pod_heartbeat' log of <pod> to [time:<numsockets>:<metrics>]

Consumes:

  * `sse_out_grep` - messages for it to write to clients!

### Grep-Streams:

This is the live worker that scours kafka for matching rows.  It keeps
track of client subscriptions to know what patterns to search for.
It's assumed that any index with an active/recent subscription is eligible
for emitting `grep_match` messages.

Consumes via (global) lookup table:

  * `sse_in_grep`
  * `grep_filters`

This allows grep-streams to know which topics to search for, and what
filters to apply (and hence, `grep_match` events to emit).

Consumes:

  * all defined/discovered kafka topics (weighted to local leader partitions)

So, grep-streams is consuming all topics and joining them against
`sse_in_grep`.  It is up to the cursor to tell whether this data is
current enough or whether a back search is required.

Produces:

  * 'grep_match' topic - log, key [topic:filter:partid:timestr] to [[]<delta offset, size>]
    - partitioned over topic:filter:jsonql (so that 'serializer' can have good affinity)
    - new searches write an empty list with the first offset searched in the topic/partition as key
    - row matches write an absolute offset and size

  * maybe `grep_match` should be a log

### Grep Stream Sequencer ("reduce")

consumes:
  * `grep_match` (partitioned over topic:filterid)

View of:
  * `grep_match_epoch` topic (also written)

emits:
  * `grep_match` topic - periodically compacted ranges and tombstones

Local State:
  * buffer of grep_match rows which could not be written to
    `grep_match_seq` because bounding epochs were not available
    (including deep scans)

Grep Stream Sequencer combines all of the `grep_match` rows for the
filter and assigns matches in streams with an epoch sequence numbers.
This allows clients to send 'last event ID' for a stream, and get
exactly once delivery of their messages.

It's possible that due to an infrequently used filter, there will be
gaps with no match data between ranges.  Empty matches written by
grep-streams are collected and once all partitions are seen, worst
case sequences can be generated (i.e., assuming that every message is
a match).

Grep Stream Sequencer produces `grep_match_seq` messages

  * `grep_match_seq` - log of [topic:filter:jsonql:epoch] to [[]<eventid:partid:offset:size>]
    - ideally partitioned over <topic:filter:jsonql>

And `epoch` for slices across topics when the '0' event (or other) for
a search happened:

  * `grep_match_epoch` - map of [topic:filter:eventid] to [time,[]<partid:offset>]

Periodic sync'ing is probably also useful.

### Grep Cursor

The grep cursor implements flow control - it follows `sse_ack_grep` and
`grep_match_seq`, and all going well, copies event stream event IDs,
message address (topic+partition+offset) & sizes to `sse_out_grep`
as the matches come in.

Things not going well, it punts requests for old ranges to grep-logs

consumes via global lookup table:
  * `sse_in_grep`

consumes/joins (partitioned over <topic:filter>):
  * `grep_match_seq`
  * `sse_ack_grep`

state:
  * bound window of `grep_match_seq` vs `sse_ack_grep` & recent history

produces:
  * `sse_out_grep` - log of <pod> to [topic:filter:jsonql,[]<eventid:partid:offset:size>,[]socketid]
     - also status/progress updates & estimates for old queries
  * `grep_log` - 'exception' when a filter is requesting a range not in the window
     - request to search by time range and topic, with possible
       exclusions/starting offsets for particular partitions which already finished.

Grep Cursor processes the `grep_match_seq` with `sse_in_grep` as a
global, and tries to send events to `sse_out_grep` that it thinks will
fit in the window.

When it reads back `ack` messages, it will continue to cue
`grep_match_seq_idx` and as messages it has sent to `sse_out_grep` are
ack'd, it can cue further.

When the grep cursor notices a previously unknown or blocked client
has window to receive, it needs to be able to look up the last ack'd
eventID, which it looks up in the 'grep match seq' table

On failover of this component, `sse_in_grep` is read from beginning
(non-compacted rows only) and `sse_ack_grep` is also recovered to see
what the client last ack'd (TCP acks).  The minimum of one SSE message
or the window size bytes divided by the number of partitions are
allowed to be outstanding (written to `sse_out_grep`, no ack seen on
`sse_ack_grep`) at any time from each `grep_match_seq` partition.

Once the client is up to date (or if it started with eventid =
latest), `grep_match_seq` rows can be sent live to the `sse_out_grep`
topic, and so long as they are ack'd then the grep cursor can relay
messages straight from `grep_match_seq` to `sse_out_grep`.

A "count" aggregating mode can be a special `sse_in_grep` flag which
simply collects statistics (matches, bucketed sizes, etc) and relays
progress update messages without sending the offsets.  With the cache
this does not have to slow down querying, other than there being an
extra delay before rows are started to be delivered.

### Grep-logs:

This worker services requests from the cursor for queries which were
not in the range of `grep_match_epoch` indexes

Consumes (lookup):
  * `sse_in_grep`
  * `grep_match_epoch`

Consumes:
  * `grep_log`

grep-logs is servicing searches which look far into the past, not
within an epoch start/stop window for a search.

Like grep-streams, allocation is over the *source* data topics and
prefers local leaders.  It services `grep_log` requests it receives,
and emits `grep_match` blocks which are close to the window size
requested in `grep_log`.  It is time bound, allowing the cursor to
walk through deep time linearly and provide a meaningful progress bar,
as well as the `grep_match_epoch` boundaries to be cleanly moved.

It can service searches from all time ranges concurrently, as each is
writing 'grep_match' index segments.  Once they overlap the results
will converge into a single bitmap.

### Grep cache

Copartitioned consumption:

  * `grep_match`
  * `grep_match_seq`

This *infrequently* commits `grep_cache`, which is a compacted
version of `grep_match` with merged blocks of `grep_match`.

This data should be committed infrequently once objects are large
enough or a deadline passes; the cursor should have the most recent
data anyway.  This needs to be used by grep-logs only.  Deadline may
need to be kept at around 5-10s or less once until proven that no live
searches will wait for it.

`grep_cache` could be indexed again by an sstable which records
strings to topic+pointers for faster recovery.

This index can be stored with variable length integer encoding, and
the 'offset' is the offset since the previous epoch.

### Grep-index:

This worker services requests from the cursor for queries which were
within `grep_match_epoch`, meaning that there are rows in
`grep_cache` for it.

Consumes (lookup):
  * `sse_in_grep`
  * `grep_match_epoch`

Consumes (partitioned over):
  * `grep_cache` (or index of it)

Consumes:
  * `grep_log`

grep-logs is servicing a client which is far enough behind that the
cursor can't service it.  The first thing it can do is to scan
`grep_cache` for the client which is behind, and if there are matches
already there which are suitable, re-send (to `grep_match_seq`) a page
of ranges to the cursor.

#### subworkers

goka consumer of grep_match_seq writing to `grep_match_seq_idx`
ktable-style store.  This is so that the grep cursor can handle sockets
which can't receive all the objects at once.

### Updog

Updog is a watchdog.  It helps by watching the heartbeat topic and
list of client subscriptions, and unsubscribing all the clients of
PODs which don't exist any more.

consumes:

* 'pod_heartbeat'

consumes (globals):

* `sse_in_grep<leveldb>`

* sends tombstones to `sse_in_grep` for pods that disappear

### Hounddog

Stuff that hounds the data and cleans it up.

#### `sse_in_grep` index aggregator

count(distinct pod:socketid) by filter+jsonql, using a tumbling window
Something like this will allow 'least frequently used' expiry, for
cleaning up the `grep_match` topic.

