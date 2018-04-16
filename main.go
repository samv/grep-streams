package main

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/confluentinc/confluent-kafka-go/kafka"
	"github.com/samv/sse"
)

// GrepStreamsAPI is a structure for a single grep-streams output channel.
// Typically one per host/pod.
type GrepStreamsAPI struct {
	sync.RWMutex

	// StreamTopic is the topic which is dedicated for grep'd events
	// to send to the client.
	StreamTopic string

	// GroupName is the quasi-unique name for the grep instance
	GroupName string

	// NodeName is the unique host/pod/IP of the GrepStreamsAPI node
	NodeName string

	// Bootstrap brokers
	Brokers []string

	// A few vars for tuning channel depths etc
	WriteChannelDepth    int
	SinkChanDepth        int
	DoneChanDepth        int
	ConsumerChannelDepth int

	// Timeout is the default timeout for the kafka consumer
	Timeout time.Duration

	// socketIDs is a sequence generator for counting new connections
	// from the same source addr
	socketIDs map[string]int

	// streams is a per-connection channel for messages to be written.
	streams map[string]*GrepStream

	// doneChan communicates streams that are closed
	doneChan chan string
}

// GrepStream is a single stream of events being grep'd to a single
// SSE channel
type GrepStream struct {
	StreamID      string
	SinkChanDepth int
	messageChan   chan *kafka.Message
	sinkChan      chan<- sse.SinkEvent
	closeChan     <-chan struct{}
	doneChan      chan<- string
}

// socketID returns a unique designator for the SSE connection by
// incrementing a counter for each time a particular remoteAddr makes
// a new request.  (with HTTP 2 all requests use the same underlying
// socket, which is the address the 'remoteAddr' refers to)
func (gsAPI *GrepStreamsAPI) socketID(remoteAddr string) int {
	gsAPI.Lock()
	if gsAPI.socketIDs == nil {
		gsAPI.socketIDs = make(map[string]int)
	}
	gsAPI.socketIDs[remoteAddr]++
	subChan := gsAPI.socketIDs[remoteAddr]
	gsAPI.Unlock()
	return subChan
}

func (gsAPI *GrepStreamsAPI) getStream(streamID string) *GrepStream {
	var stream *GrepStream
	// yeah FIXME this should be a local inside the Consume loop to avoid this lock
	gsAPI.Lock()
	if gsAPI.streams != nil {
		stream = gsAPI.streams[streamID]
	}
	gsAPI.Unlock()
	return stream
}

// ServeHTTP is the handler for grep-streams endpoints
func (gsAPI *GrepStreamsAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(fmt.Sprintf("%#v", r)))
	remoteAddr := r.RemoteAddr

	// for connect, want to get;
	//   - topic - from path
	// path := r.URL.Path
	//   - partid - need topic/source awareness
	//   - pod/socketid - assign (chanID)
	//   - filter/jsonql - from input form and/or path
	// formValues := r.URL.Query()
	//   - windowsize - spread over all connections
	var gs GrepStream
	gs.StreamID = fmt.Sprintf("%s:%s+%d", gsAPI.NodeName, remoteAddr, gsAPI.socketID(remoteAddr))
	gs.messageChan = make(chan *kafka.Message, gsAPI.WriteChannelDepth)
	gs.SinkChanDepth = gsAPI.SinkChanDepth
	gs.doneChan = gsAPI.doneChan

	// set up the write socket
	gsAPI.Lock()
	if gsAPI.streams == nil {
		gsAPI.streams = make(map[string]*GrepStream)
	}
	gsAPI.streams[gs.StreamID] = &gs
	if gsAPI.doneChan == nil {
		gsAPI.doneChan = make(chan string, gsAPI.DoneChanDepth)
	}
	gsAPI.Unlock()

	sse.SinkEvents(w, http.StatusOK, &gs)
}

// GetEventChan is a callback for making an SSE feed.  It is passed in
// a channel which is closed when it's time to stop feeding events
// (because the client has gone away).
func (gs *GrepStream) GetEventChan(clientCloseChan <-chan struct{}) <-chan sse.SinkEvent {
	sinkChan := make(chan sse.SinkEvent, gs.SinkChanDepth)
	gs.sinkChan = sinkChan
	gs.closeChan = clientCloseChan
	go gs.sink()
	return sinkChan
}

type SinkMessage struct {
	Data []byte
}

func (sm SinkMessage) GetData() ([]byte, error) {
	return sm.Data, nil
}

// sink is the loop that takes kafka messages and converts them to sinkevents.
// no backpressure for now.
func (gs *GrepStream) sink() {
	defer gs.sunk()

	var backPressureSchmackPressure []SinkMessage
	var headEvent SinkMessage
	var sinkChan chan<- sse.SinkEvent

	for {
		select {
		case <-gs.closeChan:
			// shutdown - close the sink channel and get out of here
			return
		case msg, ok := <-gs.messageChan:
			if !ok {
				// TODO - increase the 'retry' time so the client backs off
				return
			}
			// TODO - determine if this stream is overloaded and
			// update the window size to be smaller.
			backPressureSchmackPressure = append(backPressureSchmackPressure,
				SinkMessage{
					Data: msg.Value,
					// ignored, TODO: send as SSE event fields?
					// TopicPartition TopicPartition
					// Key            []byte
					// Timestamp      time.Time
					// TimestampType  TimestampType
					// Opaque         interface{}
					// Headers        []Header
				},
			)
			if len(backPressureSchmackPressure) == 1 {
				sinkChan = gs.sinkChan
				headEvent = backPressureSchmackPressure[0]
			}
		case sinkChan <- headEvent:
			backPressureSchmackPressure = backPressureSchmackPressure[1:]
			if len(backPressureSchmackPressure) == 0 {
				sinkChan = nil
			} else {
				headEvent = backPressureSchmackPressure[0]

				// TODO - determine if this stream could handle a larger window size
			}
		}
	}
}

// sunk is the cleanup function for a grep stream.
func (gs *GrepStream) sunk() {
	close(gs.sinkChan)
	gs.doneChan <- gs.StreamID
}

func (gs *GrepStream) SinkMessage(message *kafka.Message) {
	// TODO - another complicated backpressure point (backpressure to
	// the channel is not enough; it needs to be communicated to the
	// upstream writer)
	gs.messageChan <- message
}

func (gsAPI GrepStreamsAPI) TimeoutMS() int {
	var timeout int
	if gsAPI.Timeout == 0 {
		timeout = 10000
	} else {
		timeout = int(gsAPI.Timeout / time.Millisecond)
	}
	log.Printf("Returning %d for timeout", timeout)
	return timeout
}

func (gsAPI *GrepStreamsAPI) Consume(doneCB func()) {
	defer doneCB()
	consumer, err := kafka.NewConsumer(
		&kafka.ConfigMap{
			"bootstrap.servers":               strings.Join(gsAPI.Brokers, ","),
			"client.id":                       "grep-streams",
			"group.id":                        gsAPI.GroupName,
			"session.timeout.ms":              gsAPI.TimeoutMS(),
			"go.events.channel.size":          gsAPI.ConsumerChannelDepth,
			"go.events.channel.enable":        true,
			"go.application.rebalance.enable": false,
			"default.topic.config": kafka.ConfigMap{
				"auto.offset.reset": "earliest",
			},
		},
	)
	if err != nil {
		panic("Failed to start consumer: " + err.Error())
	}

	err = consumer.Subscribe(gsAPI.StreamTopic, nil)
	if err != nil {
		panic("Failed to subscribe: " + err.Error())
	}

	// TODO - allow multiple return partitions (one per pod with a
	// global assignment map, or consistent hashing from NodeName)
	consumeTopic := kafka.TopicPartitions{{Topic: &gsAPI.StreamTopic, Partition: 0}}
	err = consumer.Assign(consumeTopic)
	if err != nil {
		panic("Failed to assign partition: " + err.Error())
	}

	ourPrefix := []byte(gsAPI.NodeName + ":")

	for {
		select {
		case event, ok := <-consumer.Events():
			if !ok {
				// should I care?
				return
			}
			switch event := event.(type) {
			case *kafka.Message:
				if bytes.HasPrefix(event.Key, ourPrefix) {
					streamID := string(event.Key[len(ourPrefix):])
					stream := gsAPI.getStream(streamID)
					if stream != nil {
						stream.SinkMessage(event)
					} else {
						log.Printf("no such client: %s", string(streamID))
					}
				} else {
					log.Printf("ignoring message to key: %s", string(event.Key))
				}
				consumer.CommitMessage(event)
			default:
				log.Printf("read a non-message event: %s", event)
			}
		}
	}
}

func main() {
	grepStreamsAPI := GrepStreamsAPI{
		StreamTopic:       "greps",
		GroupName:         "grouppy",
		NodeName:          "greppy",
		Brokers:           []string{"localhost:9092"},
		WriteChannelDepth: 10,
		SinkChanDepth:     10,
		DoneChanDepth:     5,
	}
	server := http.Server{Handler: &grepStreamsAPI, Addr: ":8443"}
	go grepStreamsAPI.Consume(func() { server.Close() })
	err := server.ListenAndServeTLS("localhost.crt", "localhost.key")
	if err != nil {
		log.Printf("unclean shutdown! err=%v\n", err)
	}
}
