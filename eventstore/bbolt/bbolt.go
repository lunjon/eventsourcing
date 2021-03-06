package bbolt

import (
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/etcd-io/bbolt"
	"github.com/hallgren/eventsourcing"
	"github.com/hallgren/eventsourcing/eventstore"
	"time"
)

const (
	globalEventOrderBucketName = "global_event_order"
)

// ErrorNotFound is returned when a given entity cannot be found in the event stream
var ErrorNotFound = errors.New("NotFoundError")

// itob returns an 8-byte big endian representation of v.
func itob(v int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

// BBolt is a handler for event streaming
type BBolt struct {
	db         *bbolt.DB                  // The bbolt db where we store everything
	serializer eventstore.EventSerializer // The interface that serialize event
}

// MustOpenBBolt opens the event stream found in the given file. If the file is not found it will be created and
// initialized. Will panic if it has problems persisting the changes to the filesystem.
func MustOpenBBolt(dbFile string, s eventstore.EventSerializer) *BBolt {
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
		db:         db,
		serializer: s,
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
	bucketName := aggregateKey(aggregateType, string(aggregateID))

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
			return fmt.Errorf("could not create aggregate events bucket")
		}
		evBucket = tx.Bucket([]byte(bucketName))
	}

	currentVersion := eventsourcing.Version(0)
	cursor := evBucket.Cursor()
	k, obj := cursor.Last()
	if k != nil {
		event, err := e.serializer.DeserializeEvent(obj)
		if err != nil {
			return fmt.Errorf("could not serialize event, %v", err)
		}
		currentVersion = event.Version
	}

	//Validate events
	err = eventstore.ValidateEvents(aggregateID, currentVersion, events)
	if err != nil {
		return err
	}

	globalBucket := tx.Bucket([]byte(globalEventOrderBucketName))
	if globalBucket == nil {
		return fmt.Errorf("global bucket not found")
	}

	for _, event := range events {
		sequence, err := evBucket.NextSequence()
		if err != nil {
			return fmt.Errorf("could not get sequence for %#v", bucketName)
		}
		value, err := e.serializer.SerializeEvent(event)
		if err != nil {
			return fmt.Errorf("could not serialize event, %v", err)
		}

		err = evBucket.Put(itob(int(sequence)), value)
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
		err = globalBucket.Put(itob(int(globalSequence)), value)
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
func (e *BBolt) Get(id string, aggregateType string, afterVersion eventsourcing.Version) ([]eventsourcing.Event, error) {
	bucketName := aggregateKey(aggregateType, id)

	tx, err := e.db.Begin(false)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	evBucket := tx.Bucket([]byte(bucketName))

	cursor := evBucket.Cursor()
	events := make([]eventsourcing.Event, 0)
	firstEvent := int(afterVersion) + 1

	for k, obj := cursor.Seek(itob(firstEvent)); k != nil; k, obj = cursor.Next() {
		event, err := e.serializer.DeserializeEvent(obj)
		if err != nil {
			return nil, fmt.Errorf("Could not deserialize event, %v", err)
		}
		events = append(events, event)
	}
	return events, nil
}

// GlobalGet returns events from the global order
func (e *BBolt) GlobalGet(start int, count int) []eventsourcing.Event {
	tx, err := e.db.Begin(false)
	if err != nil {
		return nil
	}
	defer tx.Rollback()

	evBucket := tx.Bucket([]byte(globalEventOrderBucketName))
	cursor := evBucket.Cursor()
	events := make([]eventsourcing.Event, 0)
	counter := 0

	for k, obj := cursor.Seek(itob(start)); k != nil; k, obj = cursor.Next() {
		event, err := e.serializer.DeserializeEvent(obj)
		if err != nil {
			return nil
		}
		events = append(events, event)
		counter++

		if counter >= count {
			break
		}
	}
	return events
}

// Close closes the event stream and the underlying database
func (e *BBolt) Close() error {
	return e.db.Close()
}

// CreateBucket creates a bucket
func (e *BBolt) createBucket(bucketName []byte, tx *bbolt.Tx) error {
	// Ensure that we have a bucket named event_type for the given type
	if _, err := tx.CreateBucketIfNotExists([]byte(bucketName)); err != nil {
		return fmt.Errorf("could not create bucket for %s: %s", bucketName, err)
	}
	return nil

}

// aggregateKey generate a aggregate key to store events against from aggregateType and aggregateID
func aggregateKey(aggregateType, aggregateID string) string {
	return aggregateType + "_" + aggregateID
}
