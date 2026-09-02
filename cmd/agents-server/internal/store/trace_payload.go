package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"io"
	"sync"
)

// payloadFields are the span data keys held in trace_blobs rather than on
// the row: the model's request and reply, a tool's arguments and result.
// They are nearly all of a session's trace bytes — a generation span's input
// is the whole conversation — and a listing never needs them.
var payloadFields = []string{"input", "output", "system_instructions", "tools", "handoffs", "output_schema"}

// The strings a payload element is replaced with: over the per-element cap
// when written, or gone from trace_blobs when read.
const (
	PayloadCapMarker    = "[omitted: over the stored span limit (trace_span_data_kb)]"
	payloadPrunedMarker = "[omitted: the stored payload was pruned]"
)

const hashSize = sha256.Size

// layoutField is one entry of TraceEvent.Layout: field F holds N elements
// of an array, or a single element when N is -1 (a scalar).
type layoutField struct {
	F string `json:"f"`
	N int    `json:"n"`
}

func layoutTotal(layout []layoutField) int {
	n := 0
	for _, f := range layout {
		if f.N < 0 {
			n++
		} else {
			n += f.N
		}
	}
	return n
}

// splitPayload takes the payload fields out of a span's data document: the
// metadata that stays on the row, the layout, and each element's compact
// JSON with any past elemCap bytes (0: no cap) replaced by PayloadCapMarker.
// A document that is not a JSON object, or has no payload field, comes back
// as it is with a nil layout.
func splitPayload(data string, elemCap int) (meta string, layout []layoutField, elems [][]byte) {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &m) != nil || m == nil {
		return data, nil, nil
	}
	for _, f := range payloadFields {
		raw, ok := m[f]
		if !ok {
			continue
		}
		delete(m, f)
		var arr []json.RawMessage
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) && json.Unmarshal(raw, &arr) == nil {
			layout = append(layout, layoutField{F: f, N: len(arr)})
			for _, e := range arr {
				elems = append(elems, capElement(e, elemCap))
			}
			continue
		}
		layout = append(layout, layoutField{F: f, N: -1})
		elems = append(elems, capElement(raw, elemCap))
	}
	if layout == nil {
		return data, nil, nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return data, nil, nil
	}
	return string(b), layout, elems
}

// capElement compacts one element so equal values hash equal whatever their
// spacing, and replaces it with the cap marker past elemCap.
func capElement(raw json.RawMessage, elemCap int) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		buf.Reset()
		buf.Write(raw)
	}
	if elemCap > 0 && buf.Len() > elemCap {
		marker, _ := json.Marshal(PayloadCapMarker)
		return marker
	}
	return bytes.Clone(buf.Bytes())
}

// joinPayload puts the elements back into the metadata document in the
// layout's order; a nil element (its blob is gone) reads as
// payloadPrunedMarker. elems must hold layoutTotal(layout) entries.
func joinPayload(meta string, layout []layoutField, elems [][]byte) (string, error) {
	m := map[string]json.RawMessage{}
	if meta != "" {
		if err := json.Unmarshal([]byte(meta), &m); err != nil {
			return "", err
		}
		if m == nil {
			m = map[string]json.RawMessage{}
		}
	}
	pruned, _ := json.Marshal(payloadPrunedMarker)
	elem := func(i int) []byte {
		if elems[i] == nil {
			return pruned
		}
		return elems[i]
	}
	i := 0
	for _, f := range layout {
		if f.N < 0 {
			m[f.F] = elem(i)
			i++
			continue
		}
		var b bytes.Buffer
		b.WriteByte('[')
		for j := range f.N {
			if j > 0 {
				b.WriteByte(',')
			}
			b.Write(elem(i))
			i++
		}
		b.WriteByte(']')
		m[f.F] = b.Bytes()
	}
	out, err := json.Marshal(m)
	return string(out), err
}

// packRefs concatenates the elements' hashes; unpackRefs splits them again,
// false when the length is not a whole number of hashes.
func packRefs(hashes [][hashSize]byte) []byte {
	refs := make([]byte, 0, len(hashes)*hashSize)
	for _, h := range hashes {
		refs = append(refs, h[:]...)
	}
	return refs
}

func unpackRefs(refs []byte) ([][hashSize]byte, bool) {
	if len(refs)%hashSize != 0 {
		return nil, false
	}
	out := make([][hashSize]byte, len(refs)/hashSize)
	for i := range out {
		copy(out[i][:], refs[i*hashSize:])
	}
	return out, true
}

// gzipFloor is the element size below which compressing is not tried: the
// gzip framing alone is 18 bytes, and JSON this short rarely shrinks.
const gzipFloor = 256

var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

// encodeBody is how an element is stored: gzip-compressed when that is
// smaller, else as it is. JSON never starts with the gzip magic, so
// decodeBody tells the two apart without a flag.
func encodeBody(e []byte) []byte {
	if len(e) < gzipFloor {
		return e
	}
	w := gzipWriters.Get().(*gzip.Writer)
	defer gzipWriters.Put(w)
	var buf bytes.Buffer
	w.Reset(&buf)
	if _, err := w.Write(e); err != nil {
		return e
	}
	if err := w.Close(); err != nil {
		return e
	}
	if buf.Len() >= len(e) {
		return e
	}
	return buf.Bytes()
}

func decodeBody(b []byte) ([]byte, error) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, nil
	}
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}
