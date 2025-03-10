/*
 * Copyright 2017 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package table

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/AndreasBriese/bbloom"
	"github.com/infinivision/badger/options"
	"github.com/infinivision/badger/pb"
	"github.com/infinivision/badger/y"
	"github.com/pkg/errors"
)

const fileSuffix = ".sst"

// TableInterface is useful for testing.
type TableInterface interface {
	Smallest() []byte
	Biggest() []byte
	DoesNotHave(key []byte) bool
}

// Table represents a loaded table file with the info we have about it
type Table struct {
	sync.Mutex

	fd        *os.File // Own fd.
	tableSize int      // Initialized in OpenTable, using fd.Stat().

	blockIndex []*pb.BlockOffset
	ref        int32 // For file garbage collection. Atomic.

	loadingMode options.FileLoadingMode
	mmap        []byte // Memory mapped.

	// The following are initialized once and const.
	smallest, biggest []byte // Smallest and largest keys.
	id                uint64 // file id, part of filename

	bf bbloom.Bloom

	Checksum []byte
	chkMode  options.ChecksumVerificationMode // indicates when to verify checksum for blocks.
}

// IncrRef increments the refcount (having to do with whether the file should be deleted)
func (t *Table) IncrRef() {
	atomic.AddInt32(&t.ref, 1)
}

// DecrRef decrements the refcount and possibly deletes the table
func (t *Table) DecrRef() error {
	newRef := atomic.AddInt32(&t.ref, -1)
	if newRef == 0 {
		// We can safely delete this file, because for all the current files, we always have
		// at least one reference pointing to them.

		// It's necessary to delete windows files
		if t.loadingMode == options.MemoryMap {
			if err := y.Munmap(t.mmap); err != nil {
				return err
			}
		}
		if err := t.fd.Truncate(0); err != nil {
			// This is very important to let the FS know that the file is deleted.
			return err
		}
		filename := t.fd.Name()
		if err := t.fd.Close(); err != nil {
			return err
		}
		if err := os.Remove(filename); err != nil {
			return err
		}
	}
	return nil
}

type block struct {
	offset            int
	data              []byte
	numEntries        int // number of entries present in the block
	entriesIndexStart int // start index of entryOffsets list
	chkLen            int // checksum length
}

func (b block) verifyCheckSum() error {
	readPos := len(b.data) - 4 - b.chkLen
	if readPos < 0 {
		// This should be rare, hence can create a error instead of having global error.
		return fmt.Errorf("block does not contain checksum")
	}

	cs := &pb.Checksum{}
	if err := cs.Unmarshal(b.data[readPos : readPos+b.chkLen]); err != nil {
		return y.Wrapf(err, "unable to unmarshal checksum for block")
	}

	return y.VerifyChecksum(b.data[:readPos], cs)
}

func (b block) NewIterator() *blockIterator {
	bi := &blockIterator{
		data:              b.data,
		numEntries:        b.numEntries,
		entriesIndexStart: b.entriesIndexStart,
	}

	return bi
}

// OpenTable assumes file has only one table and opens it. Takes ownership of fd upon function
// entry. Returns a table with one reference count on it (decrementing which may delete the file!
// -- consider t.Close() instead). The fd has to writeable because we call Truncate on it before
// deleting. Checksum for all blocks of table is verified based on value of chkMode.
// TODO:(Ashish): convert individual args to option struct.
func OpenTable(fd *os.File, mode options.FileLoadingMode,
	chkMode options.ChecksumVerificationMode) (*Table, error) {

	fileInfo, err := fd.Stat()
	if err != nil {
		// It's OK to ignore fd.Close() errs in this function because we have only read
		// from the file.
		_ = fd.Close()
		return nil, y.Wrap(err)
	}

	filename := fileInfo.Name()
	id, ok := ParseFileID(filename)
	if !ok {
		_ = fd.Close()
		return nil, errors.Errorf("Invalid filename: %s", filename)
	}
	t := &Table{
		fd:          fd,
		ref:         1, // Caller is given one reference.
		id:          id,
		loadingMode: mode,
		chkMode:     chkMode,
	}

	t.tableSize = int(fileInfo.Size())

	if err := t.readIndex(); err != nil {
		return nil, y.Wrap(err)
	}

	it := t.NewIterator(false)
	defer it.Close()
	it.Rewind()
	if it.Valid() {
		t.smallest = it.Key()
	}

	it2 := t.NewIterator(true)
	defer it2.Close()
	it2.Rewind()
	if it2.Valid() {
		t.biggest = it2.Key()
	}

	switch mode {
	case options.LoadToRAM:
		if _, err := t.fd.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		t.mmap = make([]byte, t.tableSize)
		n, err := t.fd.Read(t.mmap)
		if err != nil {
			// It's OK to ignore fd.Close() error because we have only read from the file.
			_ = t.fd.Close()
			return nil, y.Wrapf(err, "Failed to load file into RAM")
		}
		if n != t.tableSize {
			return nil, errors.Errorf("Failed to read all bytes from the file."+
				"Bytes in file: %d Bytes actually Read: %d", t.tableSize, n)
		}
	case options.MemoryMap:
		t.mmap, err = y.Mmap(fd, false, fileInfo.Size())
		if err != nil {
			_ = fd.Close()
			return nil, y.Wrapf(err, "Unable to map file: %q", fileInfo.Name())
		}
	case options.FileIO:
		t.mmap = nil
	default:
		panic(fmt.Sprintf("Invalid loading mode: %v", mode))
	}

	if t.chkMode == options.OnTableRead || t.chkMode == options.OnTableAndBlockRead {
		if err := t.VerifyChecksum(); err != nil {
			_ = fd.Close()
			return nil, err
		}
	}

	return t, nil
}

// Close closes the open table.  (Releases resources back to the OS.)
func (t *Table) Close() error {
	if t.loadingMode == options.MemoryMap {
		if err := y.Munmap(t.mmap); err != nil {
			return err
		}
	}

	return t.fd.Close()
}

func (t *Table) read(off, sz int) ([]byte, error) {
	if len(t.mmap) > 0 {
		if len(t.mmap[off:]) < sz {
			return nil, y.ErrEOF
		}
		return t.mmap[off : off+sz], nil
	}

	res := make([]byte, sz)
	nbr, err := t.fd.ReadAt(res, int64(off))
	y.NumReads.Add(1)
	y.NumBytesRead.Add(int64(nbr))
	return res, err
}

func (t *Table) readNoFail(off, sz int) []byte {
	res, err := t.read(off, sz)
	y.Check(err)
	return res
}

func (t *Table) readIndex() error {
	readPos := t.tableSize

	// Read checksum len from the last 4 bytes.
	readPos -= 4
	buf := t.readNoFail(readPos, 4)
	checksumLen := int(binary.BigEndian.Uint32(buf))

	// Read checksum.
	expectedChk := &pb.Checksum{}
	readPos -= checksumLen
	buf = t.readNoFail(readPos, checksumLen)
	if err := expectedChk.Unmarshal(buf); err != nil {
		return err
	}

	// Read index size from the footer.
	readPos -= 4
	buf = t.readNoFail(readPos, 4)
	indexLen := int(binary.BigEndian.Uint32(buf))
	// Read index.
	readPos -= indexLen
	data := t.readNoFail(readPos, indexLen)

	if err := y.VerifyChecksum(data, expectedChk); err != nil {
		return y.Wrapf(err, "failed to verify checksum for table: %s", t.Filename())
	}

	index := pb.TableIndex{}
	err := index.Unmarshal(data)
	y.Check(err)

	t.bf = bbloom.JSONUnmarshal(index.BloomFilter)
	t.blockIndex = index.Offsets
	return nil
}

func (t *Table) block(idx int) (*block, error) {
	y.AssertTruef(idx >= 0, "idx=%d", idx)
	if idx >= len(t.blockIndex) {
		return nil, errors.New("block out of index")
	}

	ko := t.blockIndex[idx]
	blk := &block{
		offset: int(ko.Offset),
	}
	var err error
	blk.data, err = t.read(blk.offset, int(ko.Len))

	// Read meta data related to block.
	readPos := len(blk.data) - 4 // First read checksum length.
	blk.chkLen = int(binary.BigEndian.Uint32(blk.data[readPos : readPos+4]))

	// Skip reading checksum, and move position to read numEntries in block.
	readPos -= (blk.chkLen + 4)
	blk.numEntries = int(binary.BigEndian.Uint32(blk.data[readPos : readPos+4]))
	blk.entriesIndexStart = readPos - (blk.numEntries * 4)

	// Verify checksum on if checksum verification mode is OnRead on OnStartAndRead.
	if t.chkMode == options.OnBlockRead || t.chkMode == options.OnTableAndBlockRead {
		if err = blk.verifyCheckSum(); err != nil {
			return nil, err
		}
	}

	return blk, err
}

// Size is its file size in bytes
func (t *Table) Size() int64 { return int64(t.tableSize) }

// Smallest is its smallest key, or nil if there are none
func (t *Table) Smallest() []byte { return t.smallest }

// Biggest is its biggest key, or nil if there are none
func (t *Table) Biggest() []byte { return t.biggest }

// Filename is NOT the file name.  Just kidding, it is.
func (t *Table) Filename() string { return t.fd.Name() }

// ID is the table's ID number (used to make the file name).
func (t *Table) ID() uint64 { return t.id }

// DoesNotHave returns true if (but not "only if") the table does not have the key.  It does a
// bloom filter lookup.
func (t *Table) DoesNotHave(key []byte) bool { return !t.bf.Has(key) }

// VerifyChecksum verifies checksum for all blocks of table. This function is called by
// OpenTable() function. This function is also called inside levelsController.VerifyChecksum().
func (t *Table) VerifyChecksum() error {
	for i, os := range t.blockIndex {
		b, err := t.block(i)
		if err != nil {
			return y.Wrapf(err, "checksum validation failed for table: %s, block: %d, offset:%d",
				t.Filename(), i, os.Offset)
		}

		// OnBlockRead or OnTableAndBlockRead, we don't need to call verify checksum
		// on block, verification would be done while reading block itself.
		if !(t.chkMode == options.OnBlockRead || t.chkMode == options.OnTableAndBlockRead) {
			if err = b.verifyCheckSum(); err != nil {
				return y.Wrapf(err,
					"checksum validation failed for table: %s, block: %d, offset:%d",
					t.Filename(), i, os.Offset)
			}
		}
	}

	return nil
}

// ParseFileID reads the file id out of a filename.
func ParseFileID(name string) (uint64, bool) {
	name = path.Base(name)
	if !strings.HasSuffix(name, fileSuffix) {
		return 0, false
	}
	//	suffix := name[len(fileSuffix):]
	name = strings.TrimSuffix(name, fileSuffix)
	id, err := strconv.Atoi(name)
	if err != nil {
		return 0, false
	}
	y.AssertTrue(id >= 0)
	return uint64(id), true
}

// IDToFilename does the inverse of ParseFileID
func IDToFilename(id uint64) string {
	return fmt.Sprintf("%06d", id) + fileSuffix
}

// NewFilename should be named TableFilepath -- it combines the dir with the ID to make a table
// filepath.
func NewFilename(id uint64, dir string) string {
	return filepath.Join(dir, IDToFilename(id))
}
