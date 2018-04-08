
# README for grep-streams

grep-streams is a scalable, fault tolerant and highly available,
exactly once, stream processing version of grep(1).n

grep-streams is a service which provides a way to search and subscribe
to results in kafka, without being limited to reading a single topic.
It sends the filter to the data, caches popular searches, and can
resume from reconnects quickly.

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

There are three workers, each of which are distributed over the
cluster and can run on the same nodes as kafka.

### SSE Handler

This is an incoming HTTP service.  It gets the request, sends network
events to topics and consumes messages from its outbound topic.  Its
pod name is used for routing outbound messages, so a given pod only
needs to consume a single partition of `pod_eventstream`.

Produces (with ~1ms-20ms Nagle flush):

* 'client_sub' - map from [topic:partid:<pod:socketid>:filter] to [epoch:startid:jsonql:windowsize]
  - disconnect/unsubscribe - tombstone

  log.cleanup.policy=compact
  min.compaction.lag.ms - 60000 or so

* 'client_ack' - log of [topic:filter:jsonql:<pod:socketid>] value [time:eventid].  Repeats for seesion idle keepalives.

* 'pod_heartbeat' log of <pod> to [time:<numsockets>:<metrics>]

Consumes:

  * `pod_eventstream` - messages for it to write to clients!

#### `client_ack` index subworker

This subworker consumes state written to the `client_ack` topic and
writes out a sorted string table index (using leveldb).  This index
allows the write pump to be able to stick to one eventstream partition
(partition key is <topic:filter:jsonql>)

When the write pump notices a previously unknown or blocked client has
window to receive, it needs to be able to look up the last ack'd
eventID, which it looks up in the event stream index.

So, the string key is the event stream and client.

topic:filter:jsonq:<pod:socketid>

#### `client_sub` index aggregator

count(distinct pod:socketid) by filter+jsonql, using a tumbling window
Something like this will allow 'least frequently used' expiry, for
cleaning up the `matching` topic.

### Grep-Streams:

This is the main worker that scours kafka for matching rows.  It keeps
track of client subscriptions to know what to search for.  It's
assumed that any index with an active subscription is eligible for
emitting a matching bitmap for.

Consumes (each 'grep-streams' worker consumes global tables):

  * `client_sub<leveldb>`

Consumes the 'matching' index written by Serializer, below.  This is
so when it resumes, it can access the most recent 'matching' bitmap so
it knows where to resume in each partition for the filter/jsonql.

  * `matching_idx` (for validating/starting new searches)

The main consumption can first know the earliest and latest requested
index for each partition when it starts.  It can service searches from
all time ranges concurrently, as each is writing 'matching' index
segments.  Once they overlap the results will converge into a single
bitmap.

  * all defined/discovered kafka topics (weighted to local leader partitions)

For the very first '-1hr' use case, it needs to scan the partition,
binary search for the time, and then write the first 'matching'
message for the offset it found.

Produces:

  * 'matching' topic - compacted topic, key [topic:partid:filter:jsonql:epoch] to [[]<offset, size>]
    - ideally partitioned over topic:filter:jsonql (so that
      'serializer' can have good affinity)
    - can combine indices to 1k events or expire old unwanted indices
      using tombstones

This index can be stored with variable length integer encoding, and
the 'offset' is the offset since the previous epoch.

Takes key filters and data jsonql expressions and filters topics to
produce a 'bitmap' index.  It watches "client_sub" to see which indexes
need to be built.`

### Serializer

Serializer combines all of the `matching` rows for the filter and
assigns them a sequence number.  This allows clients to send 'last
event ID' for a stream, and get exactly once delivery of their
messages.

consumes (clustered over)
  * `matching` topic

produces:
  * `matching_idx` - ktable/leveldb index of 'matching' topic keys + msg_timestamp

Serializer also produces `event_stream` messages.

  * `event_stream` - log of [topic:filter:jsonql:epoch] to [[]<eventid:partid:offset:size>]
    - ideally partitioned over <topic:filter:jsonql>

#### subworkers

goka consumer of event_stream writing to `event_stream_idx`
ktable-style store.  This is so that the write pump can handle sockets
which can't receive all the objects at once.

### Writepump

The Write pump implements flow control - it follows `client_ack` and
`event_stream`, and all going well, copies event stream event IDs,
message address (topic+partition+offset) & sizes to `pod_eventstream`
as those events come in.

consumes (global map):
  * `sse_clients<leveldb>`, `client_sub<leveldb>`

consumes (lazy, cached sstable):
  * `event_stream_idx`
  * `client_ack_idx`

consumes (partitioned over):
  * `event_stream`
  * `client_ack`

produces:
  * `pod_eventstream` - log of <pod> to [topic:filter:jsonql,[]<eventid:partid:offset:size>,[]socketid]

Writepump processes the `event_stream` with `client_sub` as a global,
and tries to send events to `pod_events` that it thinks will fit in
its share of the window.  When it reads back `ack` messages, it will
continue to cue `event_stream_idx` and as messages it has sent to
pod_eventstream are ack'd, it can cue further.

On restart of this component, `client_sub` is read from beginning
(non-compacted rows only) and `client_ack` is accessed using the
ktable/LevelDB index to see what the client last ack'd (TCP acks).
The minimum of one SSE message or the window size bytes divided by the
number of partitions are allowed to be outstanding (written to
`pod_eventstream`, no ack seen on `client_ack`) at any time from each
event_stream partition.

Once the client is up to date (or if it started with eventid =
latest), event_stream rows can be sent live to the `pod_events` topic,
and so long as they are ack'd then the writepump can relay messages
straight from `event_stream` to `pod_eventstream`.

### Watchdog

Watchdog helps by watching the POD heartbeat and list of client
subscriptions, and unsubscribing all the clients of PODs which don't
exist any more.

consumes:

* 'pod_heartbeat'

consumes (globals):

* `sse_clients`
* `client_sub<leveldb>`

* sends tombstones to `sse_clients` and `client_sub` for pods that
  disappear
