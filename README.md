
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

# sse-grep

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

#### `sse_ack_grep` index subworker

This subworker consumes state written to the `sse_ack_grep` topic and
writes out a sorted string table index (using leveldb).  This index
allows the write pump to be able to stick to one eventstream partition
(partition key is <topic:filter:jsonql>)

When the write pump notices a previously unknown or blocked client has
window to receive, it needs to be able to look up the last ack'd
eventID, which it looks up in the event stream index.

So, the string key is the event stream and client.

    topic:filter:jsonq:<pod:socketid>

#### `sse_in_grep` index aggregator

count(distinct pod:socketid) by filter+jsonql, using a tumbling window
Something like this will allow 'least frequently used' expiry, for
cleaning up the `grep_match` topic.

### Grep-Streams:

This is the main worker that scours kafka for matching rows.  It keeps
track of client subscriptions to know what to search for.  It's
assumed that any index with an active subscription is eligible for
emitting a matching bitmap for.

Consumes (each 'grep-streams' worker consumes global tables):

  * `sse_in_grep<leveldb>`

Consumes the 'grep_match' index written by Grep Stream Sequencer, below.  This is
so when it resumes, it can access the most recent 'grep_match' bitmap so
it knows where to resume in each partition for the filter/jsonql.

  * `grep_match_idx` (for validating/starting new searches)

The main consumption can first know the earliest and latest requested
index for each partition when it starts.  It can service searches from
all time ranges concurrently, as each is writing 'grep_match' index
segments.  Once they overlap the results will converge into a single
bitmap.

  * all defined/discovered kafka topics (weighted to local leader partitions)

For the very first '-1hr' use case, it needs to scan the partition,
binary search for the time, and then write the first 'grep_match'
message for the offset it found.

Produces:

  * 'grep_match' topic - compacted topic, key [topic:partid:filter:jsonql:epoch] to [[]<offset, size>]
    - ideally partitioned over topic:filter:jsonql (so that
      'serializer' can have good affinity)
    - can combine indices to 1k events or expire old unwanted indices
      using tombstones

This index can be stored with variable length integer encoding, and
the 'offset' is the offset since the previous epoch.

Takes key filters and data jsonql expressions and filters topics to
produce a 'bitmap' index.  It watches "sse_in_grep" to see which indexes
need to be built.`

### Grep Stream Sequencer

Grep Stream Sequencer combines all of the `grep_out` rows for the filter and
assigns them a sequence number.  This allows clients to send 'last
event ID' for a stream, and get exactly once delivery of their
messages.

consumes (clustered over)
  * `grep_match` topic

produces:
  * `grep_match_idx` - ktable/leveldb index of 'grep_match' topic keys + msg_timestamp

Grep Stream Sequencer also produces `grep_match_seq` messages.

  * `grep_match_seq` - log of [topic:filter:jsonql:epoch] to [[]<eventid:partid:offset:size>]
    - ideally partitioned over <topic:filter:jsonql>

#### subworkers

goka consumer of grep_match_seq writing to `grep_match_seq_idx`
ktable-style store.  This is so that the write pump can handle sockets
which can't receive all the objects at once.

### Grep Cursor

The grep cursor implements flow control - it follows `sse_ack_grep` and
`grep_match_seq`, and all going well, copies event stream event IDs,
message address (topic+partition+offset) & sizes to `sse_out_grep`
as the matches come in.

consumes (global map):
  * `sse_in_grep<leveldb>`

consumes (lazy, cached sstable):
  * `grep_match_seq_idx`
  * `sse_ack_grep_idx`

consumes (partitioned over):
  * `grep_match_seq`
  * `sse_ack_grep`

produces:
  * `sse_out_grep` - log of <pod> to [topic:filter:jsonql,[]<eventid:partid:offset:size>,[]socketid]

Grep Cursor processes the `grep_match_seq` with `sse_in_grep` as a
global, and tries to send events to `sse_out_grep` that it thinks will
fit in its share of the window.  When it reads back `ack` messages, it
will continue to cue `grep_match_seq_idx` and as messages it has sent
to `sse_out_grep` are ack'd, it can cue further.

On restart of this component, `sse_in_grep` is read from beginning
(non-compacted rows only) and `sse_ack_grep` is accessed using the
ktable/LevelDB index to see what the client last ack'd (TCP acks).
The minimum of one SSE message or the window size bytes divided by the
number of partitions are allowed to be outstanding (written to
`sse_out_grep`, no ack seen on `sse_ack_grep`) at any time from each
`grep_match_seq` partition.

Once the client is up to date (or if it started with eventid =
latest), `grep_match_seq` rows can be sent live to the `sse_out_grep`
topic, and so long as they are ack'd then the writepump can relay
messages straight from `grep_match_seq` to `sse_out_grep`.

### Updog

Updog is a watchdog.  It helps by watching the heartbeat topic and
list of client subscriptions, and unsubscribing all the clients of
PODs which don't exist any more.

consumes:

* 'pod_heartbeat'

consumes (globals):

* `sse_in_grep<leveldb>`

* sends tombstones to `sse_in_grep` for pods that disappear
