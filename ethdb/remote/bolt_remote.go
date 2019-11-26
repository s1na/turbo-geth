// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package remote

import (
	"fmt"
	"io"

	"github.com/ledgerwatch/bolt"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ugorji/go/codec"
)

// Version is the current version of the remote db protocol. If the protocol changes in a non backwards compatible way,
// this constant needs to be increased
const Version uint64 = 1

// Command is the type of command in the boltdb remote protocol
type Command uint8

const (
	// CmdVersion : version
	// is sent from client to server to ask about the version of protocol the server supports
	// it is also to be used to be sent periodically to make sure the connection stays open
	CmdVersion Command = iota
	// CmdLastError request the last error generated by any command
	CmdLastError
	// CmdBeginTx : txHandle
	// request starting a new transaction (read-only). It returns transaction's handle (uint64), or 0
	// if there was an error. If 0 is returned, the corresponding error can be queried by CmdLastError command
	CmdBeginTx
	// CmdEndTx (txHandle)
	// request the end of the transaction (rollback)
	CmdEndTx
	// CmdBucket (txHandle, name): bucketHandle
	// requests opening a bucket with given name. It returns bucket's handle (uint64)
	CmdBucket
	// CmdGet (bucketHandle, key): value
	// requests a value for a key from given bucket.
	CmdGet
	// CmdCursor (bucketHandle): cursorHandle
	// request creating a cursor for the given bucket. It returns cursor's handle (uint64)
	CmdCursor
	// CmdSeek (cursorHandle, seekKey): (key, value)
	// Moves given cursor to the seekKey, or to the next key after seekKey
	CmdSeek
)

// Pool of decoders
var decoderPool = make(chan *codec.Decoder, 128)

func newDecoder(r io.Reader) *codec.Decoder {
	var d *codec.Decoder
	select {
	case d = <-decoderPool:
		d.Reset(r)
	default:
		{
			var handle codec.CborHandle
			d = codec.NewDecoder(r, &handle)
		}
	}
	return d
}

func returnDecoderToPool(d *codec.Decoder) {
	select {
	case decoderPool <- d:
	default:
		log.Warn("Allowing decoder to be garbage collected, pool is full")
	}
}

// Pool of encoders
var encoderPool = make(chan *codec.Encoder, 128)

func newEncoder(w io.Writer) *codec.Encoder {
	var e *codec.Encoder
	select {
	case e = <-encoderPool:
		e.Reset(w)
	default:
		{
			var handle codec.CborHandle
			e = codec.NewEncoder(w, &handle)
		}
	}
	return e
}

func returnEncoderToPool(e *codec.Encoder) {
	select {
	case encoderPool <- e:
	default:
		log.Warn("Allowing encoder to be garbage collected, pool is full")
	}
}

// Server is to be called as a go-routine, one per every client connection.
// It runs while the connection is active and keep the entire connection's context
// in the local variables
// For tests, bytes.Buffer can be used for both `in` and `out`
func Server(db *bolt.DB, in io.Reader, out io.Writer) error {
	decoder := newDecoder(in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(out)
	defer returnEncoderToPool(encoder)
	// Server is passive - it runs a loop what reads commands (and their arguments) and attempts to respond
	var lastError error
	var lastHandle uint64
	// Read-only transactions opened by the client
	transactions := make(map[uint64]*bolt.Tx)
	// Buckets opened by the client
	buckets := make(map[uint64]*bolt.Bucket)
	// List of buckets opened in each transaction
	bucketsByTx := make(map[uint64][]uint64)
	// Cursors opened by the client
	cursors := make(map[uint64]*bolt.Cursor)
	// List of cursors opened in each bucket
	cursorsByBucket := make(map[uint64][]uint64)
	var c Command
	for {
		if err := decoder.Decode(&c); err != nil {
			if err == io.EOF {
				// Graceful termination when the end of the input is reached
				break
			}
			log.Error("could not decode command", "error", err)
			return err
		}
		switch c {
		case CmdVersion:
			var version = Version
			if err := encoder.Encode(&version); err != nil {
				log.Error("could not encode response to CmdVersion", "error", err)
				return err
			}
		case CmdLastError:
			var errorString = fmt.Sprintf("%v", lastError)
			if err := encoder.Encode(&errorString); err != nil {
				log.Error("could not encode response to CmdLastError", "error", err)
				return err
			}
		case CmdBeginTx:
			var txHandle uint64
			var tx *bolt.Tx
			tx, lastError = db.Begin(false)
			if lastError == nil {
				defer func() {
					if err := tx.Rollback(); err != nil {
						log.Error("could not rollback transaction", "error", err)
					}
				}()
				lastHandle++
				txHandle = lastHandle
				transactions[txHandle] = tx
			}
			if err := encoder.Encode(&txHandle); err != nil {
				log.Error("could not encode txHandle in response to CmdBeginTx", "error", err)
				return err
			}
		case CmdEndTx:
			var txHandle uint64
			if err := decoder.Decode(&txHandle); err != nil {
				log.Error("could not decode txHandle for CmdEndTx")
				return err
			}
			if tx, ok := transactions[txHandle]; ok {
				// Remove all the buckets
				if bucketHandles, ok1 := bucketsByTx[txHandle]; ok1 {
					for _, bucketHandle := range bucketHandles {
						if cursorHandles, ok2 := cursorsByBucket[bucketHandle]; ok2 {
							for _, cursorHandle := range cursorHandles {
								delete(cursors, cursorHandle)
							}
							delete(cursorsByBucket, bucketHandle)
						}
						delete(buckets, bucketHandle)
					}
					delete(bucketsByTx, txHandle)
				}
				if err := tx.Rollback(); err != nil {
					log.Error("could not end transaction", "handle", txHandle, "error", err)
					return err
				}
				delete(transactions, txHandle)
				lastError = nil
			} else {
				lastError = fmt.Errorf("transaction not found")
			}
		case CmdBucket:
			// Read the txHandle
			var txHandle uint64
			if err := decoder.Decode(&txHandle); err != nil {
				log.Error("could not decode txHandle for CmdBucket")
				return err
			}
			// Read the name of the bucket
			var name []byte
			if err := decoder.Decode(&name); err != nil {
				log.Error("could not decode name for CmdBucket", "error", err)
				return err
			}
			var bucketHandle uint64
			if tx, ok := transactions[txHandle]; ok {
				// Open the bucket
				var bucket *bolt.Bucket
				bucket = tx.Bucket(name)
				if bucket == nil {
					lastError = fmt.Errorf("bucket not found")
				} else {
					lastHandle++
					bucketHandle = lastHandle
					buckets[bucketHandle] = bucket
					if bucketHandles, ok1 := bucketsByTx[txHandle]; ok1 {
						bucketHandles = append(bucketHandles, bucketHandle)
						bucketsByTx[txHandle] = bucketHandles
					} else {
						bucketsByTx[txHandle] = []uint64{bucketHandle}
					}
					lastError = nil
				}
			} else {
				lastError = fmt.Errorf("transaction not found")
			}
			if err := encoder.Encode(&bucketHandle); err != nil {
				log.Error("could not encode bucketHandle in response to CmdBucket", "error", err)
				return err
			}
		case CmdGet:
			var bucketHandle uint64
			if err := decoder.Decode(&bucketHandle); err != nil {
				log.Error("could not decode bucketHandle for CmdGet")
				return err
			}
			var key []byte
			if err := decoder.Decode(&key); err != nil {
				log.Error("could not decode key for CmdGet")
				return err
			}
			var value []byte
			if bucket, ok := buckets[bucketHandle]; ok {
				value, _ = bucket.Get(key)
				lastError = nil
			} else {
				lastError = fmt.Errorf("bucket not found")
			}
			if err := encoder.Encode(&value); err != nil {
				log.Error("could not encode value in response to CmdGet", "error", err)
				return err
			}
		case CmdCursor:
			var bucketHandle uint64
			if err := decoder.Decode(&bucketHandle); err != nil {
				log.Error("could not decode bucketHandle for CmdCursor")
				return err
			}
			var cursorHandle uint64
			if bucket, ok := buckets[bucketHandle]; ok {
				cursor := bucket.Cursor()
				lastHandle++
				cursorHandle = lastHandle
				cursors[cursorHandle] = cursor
				if cursorHandles, ok1 := cursorsByBucket[bucketHandle]; ok1 {
					cursorHandles = append(cursorHandles, cursorHandle)
					cursorsByBucket[bucketHandle] = cursorHandles
				} else {
					cursorsByBucket[bucketHandle] = []uint64{cursorHandle}
				}
				lastError = nil
			} else {
				lastError = fmt.Errorf("bucket not found")
			}
			if err := encoder.Encode(&cursorHandle); err != nil {
				log.Error("could not encode value in response to CmdCursor", "error", err)
				return err
			}
		case CmdSeek:
			var cursorHandle uint64
			if err := decoder.Decode(&cursorHandle); err != nil {
				log.Error("could not decode cursorHandle for CmdSeek")
				return err
			}
			var seekKey []byte
			if err := decoder.Decode(&seekKey); err != nil {
				log.Error("could not decode seekKey for CmdSeek")
				return err
			}
		default:
			log.Error("unknown", "command", c)
			return fmt.Errorf("unknown command %d", c)
		}
	}
	return nil
}
