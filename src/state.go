package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/emersion/go-imap/v2"
	bolt "go.etcd.io/bbolt"
)

const stateLockTimeout = 5 * time.Second

var (
	sourcesBucket  = []byte("sources")
	uidsBucket     = []byte("uids")
	uidValidityKey = []byte("uidvalidity")
)

// StateStore remembers which messages were already forwarded, for sources
// whose IMAP server does not keep the $Forwarded flag between connections.
//
// Layout: sources/<trackingID>/<folder>/ holds the folder's UIDVALIDITY under
// "uidvalidity" and a "uids" bucket mapping each forwarded UID to the time it
// was forwarded.
type StateStore struct {
	db *bolt.DB
}

// OpenStateStore opens (creating if needed) the state database at path.
func OpenStateStore(path string) (*StateStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: stateLockTimeout})
	if err != nil {
		return nil, fmt.Errorf("open state file %q: %w", path, err)
	}
	return &StateStore{db: db}, nil
}

// Close closes the underlying database.
func (s *StateStore) Close() error {
	return s.db.Close()
}

// SyncUIDValidity records the folder's UIDVALIDITY. If a different value was
// stored before, the UIDs of that folder are no longer meaningful, so they are
// cleared and reset is true.
func (s *StateStore) SyncUIDValidity(trackingID, folder string, validity uint32) (reset bool, err error) {
	want := make([]byte, 4)
	binary.BigEndian.PutUint32(want, validity)

	err = s.db.Update(func(tx *bolt.Tx) error {
		fb, err := folderBucket(tx, trackingID, folder, true)
		if err != nil {
			return err
		}
		stored := fb.Get(uidValidityKey)
		if bytes.Equal(stored, want) {
			return nil
		}
		if stored != nil {
			if err := fb.DeleteBucket(uidsBucket); err != nil && !errors.Is(err, bolt.ErrBucketNotFound) {
				return err
			}
			reset = true
		}
		return fb.Put(uidValidityKey, want)
	})
	return reset, err
}

// FilterUnforwarded returns the UIDs that have not been recorded as forwarded,
// preserving their order.
func (s *StateStore) FilterUnforwarded(trackingID, folder string, uids []imap.UID) ([]imap.UID, error) {
	pending := make([]imap.UID, 0, len(uids))
	err := s.db.View(func(tx *bolt.Tx) error {
		var ub *bolt.Bucket
		if fb, err := folderBucket(tx, trackingID, folder, false); err != nil {
			return err
		} else if fb != nil {
			ub = fb.Bucket(uidsBucket)
		}
		for _, uid := range uids {
			if ub == nil || ub.Get(uidKey(uid)) == nil {
				pending = append(pending, uid)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

// MarkForwarded records that uid was forwarded. It is idempotent.
func (s *StateStore) MarkForwarded(trackingID, folder string, uid imap.UID) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		fb, err := folderBucket(tx, trackingID, folder, true)
		if err != nil {
			return err
		}
		ub, err := fb.CreateBucketIfNotExists(uidsBucket)
		if err != nil {
			return err
		}
		return ub.Put(uidKey(uid), []byte(time.Now().UTC().Format(time.RFC3339)))
	})
}

// folderBucket returns the bucket for trackingID/folder. With create false it
// returns nil (and no error) when the bucket does not exist.
func folderBucket(tx *bolt.Tx, trackingID, folder string, create bool) (*bolt.Bucket, error) {
	if !create {
		root := tx.Bucket(sourcesBucket)
		if root == nil {
			return nil, nil
		}
		src := root.Bucket([]byte(trackingID))
		if src == nil {
			return nil, nil
		}
		return src.Bucket([]byte(folder)), nil
	}

	root, err := tx.CreateBucketIfNotExists(sourcesBucket)
	if err != nil {
		return nil, err
	}
	src, err := root.CreateBucketIfNotExists([]byte(trackingID))
	if err != nil {
		return nil, err
	}
	return src.CreateBucketIfNotExists([]byte(folder))
}

func uidKey(uid imap.UID) []byte {
	key := make([]byte, 4)
	binary.BigEndian.PutUint32(key, uint32(uid))
	return key
}
