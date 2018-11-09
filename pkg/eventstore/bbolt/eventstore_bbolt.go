package bbolt

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go-event-sourcing-sample/pkg/eventsourcing"
	"go-event-sourcing-sample/pkg/eventstore"
	"strconv"
	"time"

	"github.com/etcd-io/bbolt"
)

const (
	globalEventOrderBucketName = "global_event_order"
)

// NotFoundError is returned when a given entity cannot be found in the event stream
var NotFoundError = errors.New("NotFoundError")

// itob returns an 8-byte big endian representation of v.
func itob(v int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

// BBolt is a handler for event streaming
type BBolt struct {
	db *bbolt.DB // The bbolt db where we store everything
}

// MustOpenBBolt opens the event stream found in the given file. If the file is not found it will be created and
// initialized. Will panic if it has problems persisting the changes to the filesystem.
func MustOpenBBolt(dbFile string) *BBolt {
	db, err := bbolt.Open(dbFile, 0600, &bbolt.Options{
		Timeout: 1 * time.Second,
	})
	if err != nil {
		panic(err)
	}

	// Ensure that we have a bucket to store the global event ordering
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(globalEventOrderBucketName)); err != nil {
			return fmt.Errorf("could not create global event order bucket")
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	return &BBolt{
		db: db,
	}
}

// Save an aggregate (its events)
func (e *BBolt) Save(events []eventsourcing.Event) error {
	// Return if there is no events to save
	if len(events) == 0 {
		return nil
	}

	// get bucket name from first event
	aggregateType := events[0].AggregateType
	aggregateID := events[0].AggregateRootID
	bucketName := bucketName(aggregateType, string(aggregateID))

	tx, err := e.db.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	evBucket := tx.Bucket([]byte(bucketName))
	if evBucket == nil {
		// Ensure that we have a bucket named events_aggregateType_aggregateID for the given aggregate
		err = e.createBucket([]byte(bucketName), tx)
		if err != nil {
			return fmt.Errorf("EXIT SAVE")
		}
		evBucket = tx.Bucket([]byte(bucketName))
	}

	currentVersion := eventsourcing.Version(0)
	cursor := evBucket.Cursor()
	k, obj := cursor.Last()
	if k != nil {
		jsonObj := eventstore.MustDecompress(obj)
		// UnMarshal the json object
		var event = eventsourcing.Event{}
		err := json.Unmarshal(jsonObj, &event)
		if err != nil {
			return err
		}
		// Last version in the list
		currentVersion = event.Version
	}

	//Validate events
	ok, err := e.validateEvents(aggregateID, currentVersion, events)
	if !ok {
		//TODO created describing errors
		return err
	}

	globalBucket := tx.Bucket([]byte(globalEventOrderBucketName))
	if globalBucket == nil {
		return fmt.Errorf("global bucket not found")
	}

	for _, event := range events {
		// Marshal the event object for saving to the database
		obj, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("could not marshal delta object for %#v", obj)
		}

		sequence, err := evBucket.NextSequence()
		if err != nil {
			return fmt.Errorf("could not get sequence for %#v", bucketName)
		}

		err = evBucket.Put(itob(int(sequence)), eventstore.MustCompress(obj))
		if err != nil {
			return fmt.Errorf("could not save event %#v in bucket", event)
		}
		// We need to establish a global event order that spans over all buckets. This is so that we can be
		// able to play the event (or send) them in the order that they was entered into this database.
		// The global sequence bucket contains an ordered line of pointer to all events on the form bucket_name:seq_num
		globalSequence, err := globalBucket.NextSequence()
		if err != nil {
			return fmt.Errorf("could not get next sequence for global bucket")
		}
		globalSequenceValue := bucketName + ":" + strconv.FormatUint(sequence, 10)
		err = globalBucket.Put(itob(int(globalSequence)), []byte(globalSequenceValue))
		if err != nil {
			return fmt.Errorf("could not save global sequence pointer for %#v", bucketName)
		}

	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	return nil
}

// Get aggregate events
func (e *BBolt) Get(id string, aggregateType string) ([]eventsourcing.Event, error) {
	fmt.Println("GET")
	bucketName := bucketName(aggregateType, id)

	tx, err := e.db.Begin(false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	evBucket := tx.Bucket([]byte(bucketName))

	cursor := evBucket.Cursor()
	events := []eventsourcing.Event{}
	var event = eventsourcing.Event{}

	for k, obj := cursor.First(); k != nil; k, obj = cursor.Next() {
		jsonObj := eventstore.MustDecompress(obj)
		// UnMarshal the json object
		err := json.Unmarshal(jsonObj, &event)
		if err != nil {
			return nil, fmt.Errorf("could not unmarshal json object for %#v", obj)
		}
		events = append(events, eventsourcing.Event(event))
	}
	return events, nil
}

// CreateBucket creates a bucket
func (e *BBolt) createBucket(bucketName []byte, tx *bbolt.Tx) error {
	// Ensure that we have a bucket named event_type for the given type
	if _, err := tx.CreateBucketIfNotExists([]byte(bucketName)); err != nil {
		return fmt.Errorf("could not create bucket for %s: %s", bucketName, err)
	}
	return nil

}

// Close closes the event stream and the underlying database
func (e *BBolt) Close() error {
	return e.db.Close()
}

func bucketName(aggregateType, aggregateID string) string {
	return aggregateType + "_" + aggregateID
}

func (e *BBolt) validateEvents(aggregateID eventsourcing.AggregateRootID, currentVersion eventsourcing.Version, events []eventsourcing.Event) (bool, error) {
	aggregateType := events[0].AggregateType

	for _, event := range events {
		if event.AggregateRootID != aggregateID {
			return false, fmt.Errorf("events holds events for more than one aggregate")
		}

		if event.AggregateType != aggregateType {
			return false, fmt.Errorf("events holds events for more than one aggregate type")
		}

		if currentVersion+1 != event.Version {
			return false, fmt.Errorf("concurrency error")
		}

		if event.Reason == "" {
			return false, fmt.Errorf("event holds no reason")
		}

		currentVersion = event.Version
	}
	return true, nil
}