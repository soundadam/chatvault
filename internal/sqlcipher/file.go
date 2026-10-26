package sqlcipher

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WAL layout (https://www.sqlite.org/fileformat2.html#walformat): a 32-byte
// header, then frames of a 24-byte header plus one page. SQLCipher encrypts
// the page before SQLite checksums the frame, so the checksum chain can be
// checked without the key.
const (
	walHeaderLen = 32
	walFrameHdr  = 24
)

// walPages returns, for every page the WAL holds in a committed transaction,
// the file offset of its newest frame, and the database size in pages after
// the last commit (0 if nothing is committed).
func walPages(wal []byte) (map[uint32]int64, uint32, error) {
	pages := map[uint32]int64{}
	if len(wal) < walHeaderLen {
		return pages, 0, nil
	}
	magic := binary.BigEndian.Uint32(wal)
	if magic&^1 != 0x377f0682 {
		return nil, 0, errors.New("sqlcipher: bad WAL magic")
	}
	if ps := binary.BigEndian.Uint32(wal[8:]); ps != PageSize {
		return nil, 0, fmt.Errorf("sqlcipher: WAL page size %d", ps)
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if magic&1 == 1 {
		order = binary.BigEndian
	}
	salt1, salt2 := binary.BigEndian.Uint32(wal[16:]), binary.BigEndian.Uint32(wal[20:])
	s0, s1 := walChecksum(order, wal[:24], 0, 0)
	if s0 != binary.BigEndian.Uint32(wal[24:]) || s1 != binary.BigEndian.Uint32(wal[28:]) {
		return pages, 0, nil
	}

	pending := map[uint32]int64{}
	var size uint32
	for off := int64(walHeaderLen); off+walFrameHdr+PageSize <= int64(len(wal)); off += walFrameHdr + PageSize {
		h := wal[off : off+walFrameHdr]
		if binary.BigEndian.Uint32(h[8:]) != salt1 || binary.BigEndian.Uint32(h[12:]) != salt2 {
			break
		}
		s0, s1 = walChecksum(order, h[:8], s0, s1)
		s0, s1 = walChecksum(order, wal[off+walFrameHdr:off+walFrameHdr+PageSize], s0, s1)
		if s0 != binary.BigEndian.Uint32(h[16:]) || s1 != binary.BigEndian.Uint32(h[20:]) {
			break
		}
		pending[binary.BigEndian.Uint32(h)] = off + walFrameHdr
		if commit := binary.BigEndian.Uint32(h[4:]); commit != 0 {
			for p, o := range pending {
				pages[p] = o
			}
			clear(pending)
			size = commit
		}
	}
	return pages, size, nil
}

func walChecksum(order binary.ByteOrder, b []byte, s0, s1 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += order.Uint32(b[i:]) + s1
		s1 += order.Uint32(b[i+4:]) + s0
	}
	return s0, s1
}

// DecryptFile writes the plain form of the encrypted database at src, with
// the committed frames of src+"-wal" folded in, to dst (atomically, through
// a temporary file next to it). key is the page key from MatchKey.
func DecryptFile(src, dst string, key []byte) error {
	db, err := os.Open(src)
	if err != nil {
		return err
	}
	defer db.Close()
	st, err := db.Stat()
	if err != nil {
		return err
	}
	page := make([]byte, PageSize)
	if _, err := io.ReadFull(db, page); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	c, err := New(key, Salt(page))
	if err != nil {
		return err
	}

	wal, err := os.ReadFile(src + "-wal")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	walIdx, walSize, err := walPages(wal)
	if err != nil {
		return fmt.Errorf("%s-wal: %w", src, err)
	}
	npages := uint32(st.Size() / PageSize)
	if walSize != 0 {
		npages = walSize
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	plain := make([]byte, PageSize)
	for pgno := uint32(1); pgno <= npages; pgno++ {
		src := page
		if off, ok := walIdx[pgno]; ok {
			src = wal[off : off+PageSize]
		} else if _, err := db.ReadAt(page, int64(pgno-1)*PageSize); err != nil {
			out.Close()
			return fmt.Errorf("%s: page %d: %w", db.Name(), pgno, err)
		}
		if err := c.Decrypt(plain, src, pgno); err != nil {
			out.Close()
			return fmt.Errorf("%s: %w", db.Name(), err)
		}
		if _, err := out.Write(plain); err != nil {
			out.Close()
			return err
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}
