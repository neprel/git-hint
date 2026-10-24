package runner

import (
	"errors"
	"io"
)

const tarBlockBytes = 512

// treeArchiveReader bounds all source bytes, including extension headers and trailer
// padding. archive/tar alone accepts physical EOF at an entry boundary without tar EOS.
// The tail retains only enough bytes to distinguish that EOF from the complete marker.
type treeArchiveReader struct {
	source      io.Reader
	limit, read int64
	padding     int64
	tail        []byte
	failure     error
}

func (r *treeArchiveReader) Read(p []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	remaining := r.limit - r.read
	if remaining < 0 {
		return 0, errors.New("source archive too large")
	}
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := r.source.Read(p)
	r.read += int64(n)
	checked := int64(n)
	if checked > r.padding {
		checked = r.padding
	}
	if !zeroTarBytes(p[:checked]) {
		r.failure = errors.New("nonzero tar payload padding")
	}
	r.padding -= checked
	const window = 3 * tarBlockBytes
	if n >= window {
		r.tail = append(r.tail[:0], p[n-window:n]...)
	} else {
		if extra := len(r.tail) + n - window; extra > 0 {
			copy(r.tail, r.tail[extra:])
			r.tail = r.tail[:len(r.tail)-extra]
		}
		r.tail = append(r.tail, p[:n]...)
	}
	if r.read > r.limit {
		r.failure = errors.New("source archive too large")
	}
	if r.failure != nil {
		return n, r.failure
	}
	return n, err
}

func zeroTarBytes(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}

// finish requires the two complete zero end blocks that Next consumed, after the last
// payload's padding. No entry-boundary EOF, partial marker, dangling extension header,
// nonzero padding, concatenated archive or nonzero trailing content is a complete tree.
// Extra complete zero blocks (e.g. GNU record padding) remain valid under the raw bound.
func (r *treeArchiveReader) finish(before, padding int64) error {
	consumed := r.read - before
	if r.failure != nil {
		return r.failure
	}
	if consumed != padding+2*tarBlockBytes || consumed > int64(len(r.tail)) || r.padding != 0 || !zeroTarBytes(r.tail[int64(len(r.tail))-consumed:]) {
		return errors.New("incomplete tar end marker")
	}
	var block [tarBlockBytes]byte
	for {
		n, err := io.ReadFull(r, block[:])
		if err == io.EOF && n == 0 {
			return nil
		}
		if err != nil {
			return errors.New("incomplete or oversized tar trailer")
		}
		if !zeroTarBytes(block[:]) {
			return errors.New("nonzero tar trailer")
		}
	}
}
