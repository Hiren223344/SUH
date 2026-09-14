// Package jsonutil provides an order-preserving JSON object representation.
//
// The router treats every client- and upstream-facing JSON object (chat
// completion requests, responses, SSE chunks, error envelopes) as an
// OrderedMap rather than decoding into a fixed struct. This lets the
// sanitize chokepoint rewrite or strip a handful of known keys (model, id,
// usage) while passing every other key through byte-for-byte, in the order
// the upstream sent it — which matters because some SDKs read fields
// positionally out of streamed SSE chunks.
package jsonutil

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// OrderedMap is a JSON object that preserves key insertion/decode order.
type OrderedMap struct {
	keys []string
	vals map[string]json.RawMessage
}

// NewOrderedMap returns an empty OrderedMap.
func NewOrderedMap() *OrderedMap {
	return &OrderedMap{vals: map[string]json.RawMessage{}}
}

// ParseObject decodes data (which must be a JSON object) into an OrderedMap.
func ParseObject(data []byte) (*OrderedMap, error) {
	m := &OrderedMap{}
	if err := m.UnmarshalJSON(data); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *OrderedMap) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("jsonutil: expected JSON object, got %v", tok)
	}
	m.keys = nil
	m.vals = map[string]json.RawMessage{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("jsonutil: expected string key, got %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		if _, exists := m.vals[key]; !exists {
			m.keys = append(m.keys, key)
		}
		m.vals[key] = raw
	}
	// consume closing '}'
	if _, err := dec.Token(); err != nil {
		return err
	}
	return nil
}

func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		v := m.vals[k]
		if len(v) == 0 {
			buf.WriteString("null")
		} else {
			buf.Write(v)
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Get returns the raw JSON value for key, if present.
func (m *OrderedMap) Get(key string) (json.RawMessage, bool) {
	if m.vals == nil {
		return nil, false
	}
	v, ok := m.vals[key]
	return v, ok
}

// GetString returns key's value unmarshaled as a string, if present and a string.
func (m *OrderedMap) GetString(key string) (string, bool) {
	raw, ok := m.Get(key)
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// Has reports whether key is present (including if its value is JSON null).
func (m *OrderedMap) Has(key string) bool {
	_, ok := m.Get(key)
	return ok
}

// Set inserts or updates key with a raw JSON value. New keys are appended to
// the end; existing keys keep their position.
func (m *OrderedMap) Set(key string, value json.RawMessage) {
	if m.vals == nil {
		m.vals = map[string]json.RawMessage{}
	}
	if _, exists := m.vals[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.vals[key] = value
}

// SetValue marshals v and stores it under key.
func (m *OrderedMap) SetValue(key string, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	m.Set(key, b)
	return nil
}

// Delete removes key, if present.
func (m *OrderedMap) Delete(key string) {
	if _, exists := m.vals[key]; !exists {
		return
	}
	delete(m.vals, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
}

// Keys returns the keys in their current order. Callers must not mutate it.
func (m *OrderedMap) Keys() []string {
	return m.keys
}

// KeepOnly deletes every key not in allow.
func (m *OrderedMap) KeepOnly(allow map[string]bool) {
	for _, k := range append([]string(nil), m.keys...) {
		if !allow[k] {
			m.Delete(k)
		}
	}
}

// Clone returns a deep-enough copy (raw values are immutable byte slices, so
// sharing them is safe; only the key slice/map are copied).
func (m *OrderedMap) Clone() *OrderedMap {
	c := &OrderedMap{
		keys: append([]string(nil), m.keys...),
		vals: make(map[string]json.RawMessage, len(m.vals)),
	}
	for k, v := range m.vals {
		c.vals[k] = v
	}
	return c
}
