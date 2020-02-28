// Copyright 2017 The Celo Authors
// This file is part of the celo library.
//
// The celo library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The celo library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the celo library. If not, see <http://www.gnu.org/licenses/>.

package enodes

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/istanbul"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rlp"
)

// // Keys in the node database.
// const (
// 	dbVersionKey    = "version"  // Version of the database to flush if changes
// 	dbAddressPrefix = "address:" // Identifier to prefix address keys with
// )

const (
	// dbNodeExpiration = 24 * time.Hour // Time after which an unseen node should be dropped.
	// dbCleanupCycle   = time.Hour      // Time period for running the expiration task.
	dbVersionSignedAnnounceVersion = 0
)

// SignedAnnounceVersionDB represents a Map that can be accessed either
// by address or enode
type SignedAnnounceVersionDB struct {
	db           *leveldb.DB //the actual DB
	logger       log.Logger
	writeOptions *opt.WriteOptions
}

// SignedAnnounceVersionEntry is an entry
type SignedAnnounceVersion struct {
	Address   common.Address
	Version   uint
	Signature []byte
}

type SignedAnnounceVersionEntry struct {
	*SignedAnnounceVersion
	Timestamp time.Time
}

// EncodeRLP serializes announceVersion into the Ethereum RLP format.
func (sve *SignedAnnounceVersionEntry) EncodeRLP(w io.Writer) error {
	return rlp.Encode(w, []interface{}{sve.Address, sve.Version, sve.Signature, sve.Timestamp})
}

// DecodeRLP implements rlp.Decoder, and load the announceVerion fields from a RLP stream.
func (sve *SignedAnnounceVersionEntry) DecodeRLP(s *rlp.Stream) error {
	var msg struct {
		Address   common.Address
		Version   uint
		Signature []byte
		Timestamp time.Time
	}

	if err := s.Decode(&msg); err != nil {
		return err
	}
	sve.Address, sve.Version, sve.Signature, sve.Timestamp = msg.Address, msg.Version, msg.Signature, msg.Timestamp
	return nil
}

func (sve *SignedAnnounceVersionEntry) String() string {
	return fmt.Sprintf("{Address: %v, Version: %v, Signature.length: %v, Timestamp: %v}", sve.Address, sve.Version, len(sve.Signature), sve.Timestamp)
}

func (sv *SignedAnnounceVersion) ValidateSignature() error {
	signedAnnounceVersionNoSig := &SignedAnnounceVersion{
		Address: sv.Address,
		Version: sv.Version,
	}
	bytesNoSignature, err := rlp.EncodeToBytes(signedAnnounceVersionNoSig)
	if err != nil {
		return err
	}
	address, err := istanbul.GetSignatureAddress(bytesNoSignature, sv.Signature)
	if err != nil {
		return err
	}
	if address != sv.Address {
		return errors.New("Signature does not match address")
	}
	return nil
}

// OpenSignedAnnounceVersionDB opens a signed announce version database for storing
// signedAnnounceVersions. If no path is given an in-memory, temporary database is constructed.
func OpenSignedAnnounceVersionDB(path string) (*SignedAnnounceVersionDB, error) {
	var db *leveldb.DB
	var err error

	logger := log.New("db", "SignedAnnounceVersionDB")

	if path == "" {
		db, err = newMemoryDB()
	} else {
		db, err = newPersistentDB(path, logger)
	}

	if err != nil {
		return nil, err
	}
	return &SignedAnnounceVersionDB{
		db:      db,
		logger:  logger,
		writeOptions: &opt.WriteOptions{NoWriteMerge: true},
	}, nil
}

// Close flushes and closes the database files.
func (svdb *SignedAnnounceVersionDB) Close() error {
	return svdb.db.Close()
}

func (svdb *SignedAnnounceVersionDB) String() string {
	var b strings.Builder
	b.WriteString("ValEnodeTable:")

	err := svdb.iterateOverAddressEntries(func(address common.Address, entry *SignedAnnounceVersionEntry) error {
		fmt.Fprintf(&b, " [%s => %s]", address.String(), entry.String())
		return nil
	})

	if err != nil {
		svdb.logger.Error("ValidatorEnodeDB.String error", "err", err)
	}

	return b.String()
}

// GetVersionFromAddress will return the version for an address if it's known
func (svdb *SignedAnnounceVersionDB) GetVersionFromAddress(address common.Address) (uint, error) {
	entry, err := svdb.getEntry(address)
	if err != nil {
		return 0, err
	}
	return entry.Version, nil
}

// Upsert inserts any new entries or entries with a Version higher than the
// existing version. Returns if there were any new or updated entries
func (svdb *SignedAnnounceVersionDB) Upsert(signedAnnounceVersions []*SignedAnnounceVersion) (bool, error) {
    logger := svdb.logger.New("func", "Upsert")
	batch := new(leveldb.Batch)

	newEntries := false

    for _, signedAnnVersion := range signedAnnounceVersions {
        currentEntry, err := svdb.getEntry(signedAnnVersion.Address)
        isNew := err == leveldb.ErrNotFound
		if !isNew && err != nil {
			return false, err
		}
        if !isNew && signedAnnVersion.Version <= currentEntry.Version {
            logger.Trace("Not inserting, version is not greater than the existing entry",
                "address", signedAnnVersion.Address, "existing version", currentEntry.Version,
                "new entry version", signedAnnVersion.Version)
            continue
        }
		entry := SignedAnnounceVersionEntry{
			SignedAnnounceVersion: signedAnnVersion,
			Timestamp: time.Now(),
		}
        entryBytes, err := rlp.EncodeToBytes(entry)
        if err != nil {
            return false, err
        }
        batch.Put(addressKey(signedAnnVersion.Address), entryBytes)
		newEntries = true
        logger.Trace("Updating with new entry", "isNew", isNew,
            "address", signedAnnVersion.Address, "new version", signedAnnVersion.Version)
    }

    if batch.Len() > 0 {
        err := svdb.db.Write(batch, svdb.writeOptions)
        if err != nil {
            return false, err
        }
    }
    return newEntries, nil
}

// GetAllEntries gets all entries in the db
func (svdb *SignedAnnounceVersionDB) GetAllEntries() ([]*SignedAnnounceVersionEntry, error) {
	var entries []*SignedAnnounceVersionEntry
	err := svdb.iterateOverAddressEntries(func(address common.Address, entry *SignedAnnounceVersionEntry) error {
		entries = append(entries, entry)
		return nil
	})
	return entries, err
}

// GetAllSignedAnnounceVersions gets all SignedAnnounceVersions in the db
func (svdb *SignedAnnounceVersionDB) GetAllSignedAnnounceVersions() ([]*SignedAnnounceVersion, error) {
	var signedAnnounceVersions []*SignedAnnounceVersion
	err := svdb.iterateOverAddressEntries(func(address common.Address, entry *SignedAnnounceVersionEntry) error {
		signedAnnounceVersions = append(signedAnnounceVersions, entry.SignedAnnounceVersion)
		return nil
	})
	return signedAnnounceVersions, err
}

// RemoveEntry will remove an entry from the table
func (svdb *SignedAnnounceVersionDB) RemoveEntry(address common.Address) error {
	batch := new(leveldb.Batch)
	batch.Delete(addressKey(address))
	return svdb.db.Write(batch, svdb.writeOptions)
}

// PruneEntries will remove entries for all address not present in addressesToKeep
func (svdb *SignedAnnounceVersionDB) PruneEntries(addressesToKeep map[common.Address]bool) error {
	batch := new(leveldb.Batch)
	err := svdb.iterateOverAddressEntries(func(address common.Address, entry *SignedAnnounceVersionEntry) error {
		if !addressesToKeep[address] {
			svdb.logger.Trace("Deleting entry", "address", address)
			batch.Delete(addressKey(address))
		}
		return nil
	})
	if err != nil {
		return err
	}
	return svdb.db.Write(batch, svdb.writeOptions)
}

func (svdb *SignedAnnounceVersionDB) getEntry(address common.Address) (*SignedAnnounceVersionEntry, error) {
	var entry SignedAnnounceVersionEntry
	rawEntry, err := svdb.db.Get(addressKey(address), nil)
	if err != nil {
		return nil, err
	}

	if err = rlp.DecodeBytes(rawEntry, &entry); err != nil {
		return nil, err
	}
	return &entry, nil
}

func (svdb *SignedAnnounceVersionDB) iterateOverAddressEntries(onEntry func(common.Address, *SignedAnnounceVersionEntry) error) error {
	iter := svdb.db.NewIterator(util.BytesPrefix([]byte(dbAddressPrefix)), nil)
	defer iter.Release()

	for iter.Next() {
		var entry SignedAnnounceVersionEntry
		address := common.BytesToAddress(iter.Key()[len(dbAddressPrefix):])
		rlp.DecodeBytes(iter.Value(), &entry)

		err := onEntry(address, &entry)
		if err != nil {
			return err
		}
	}
	return iter.Error()
}

// SignedAnnounceVersionEntryInfo todo comment
type SignedAnnounceVersionEntryInfo struct {
	Address string `json:"address"`
	Version uint   `json:"version"`
}

// Info todo comment
func (svdb *SignedAnnounceVersionDB) Info() (map[string]*SignedAnnounceVersionEntryInfo, error) {
	dbInfo := make(map[string]*SignedAnnounceVersionEntryInfo)
	err := svdb.iterateOverAddressEntries(func(address common.Address, entry *SignedAnnounceVersionEntry) error {
		dbInfo[address.Hex()] = &SignedAnnounceVersionEntryInfo{
			Address: entry.Address.Hex(),
			Version: entry.Version,
		}
		return nil
	})
	return dbInfo, err
}