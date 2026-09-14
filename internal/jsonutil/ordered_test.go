package jsonutil

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrderedMap_PreservesKeyOrder(t *testing.T) {
	raw := []byte(`{"z":1,"a":2,"m":3,"b":4}`)
	m, err := ParseObject(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"z", "a", "m", "b"}, m.Keys())

	out, err := m.MarshalJSON()
	require.NoError(t, err)
	require.Equal(t, `{"z":1,"a":2,"m":3,"b":4}`, string(out))
}

func TestOrderedMap_SetAppendsNewKeysAtEndAndKeepsPositionForExisting(t *testing.T) {
	m, err := ParseObject([]byte(`{"a":1,"b":2}`))
	require.NoError(t, err)

	require.NoError(t, m.SetValue("c", 3))  // new key -> appended
	require.NoError(t, m.SetValue("a", 99)) // existing key -> position kept

	require.Equal(t, []string{"a", "b", "c"}, m.Keys())
	out, _ := m.MarshalJSON()
	require.Equal(t, `{"a":99,"b":2,"c":3}`, string(out))
}

func TestOrderedMap_Delete(t *testing.T) {
	m, err := ParseObject([]byte(`{"a":1,"b":2,"c":3}`))
	require.NoError(t, err)
	m.Delete("b")
	require.Equal(t, []string{"a", "c"}, m.Keys())
	require.False(t, m.Has("b"))

	// Deleting a nonexistent key is a no-op, not an error.
	m.Delete("nonexistent")
	require.Equal(t, []string{"a", "c"}, m.Keys())
}

func TestOrderedMap_KeepOnly(t *testing.T) {
	m, err := ParseObject([]byte(`{"a":1,"b":2,"c":3,"d":4}`))
	require.NoError(t, err)
	m.KeepOnly(map[string]bool{"b": true, "d": true})
	require.ElementsMatch(t, []string{"b", "d"}, m.Keys())
}

func TestOrderedMap_CloneIsIndependent(t *testing.T) {
	m, err := ParseObject([]byte(`{"a":1}`))
	require.NoError(t, err)
	clone := m.Clone()
	require.NoError(t, clone.SetValue("b", 2))

	require.False(t, m.Has("b"), "mutating a clone must not affect the original")
	require.True(t, clone.Has("b"))
}

func TestOrderedMap_GetStringAndHas(t *testing.T) {
	m, err := ParseObject([]byte(`{"model":"gpt-router","n":null}`))
	require.NoError(t, err)
	s, ok := m.GetString("model")
	require.True(t, ok)
	require.Equal(t, "gpt-router", s)

	_, ok = m.GetString("missing")
	require.False(t, ok)

	require.True(t, m.Has("n"), "a key present with JSON null value must still count as present")
}

func TestOrderedMap_RejectsNonObject(t *testing.T) {
	_, err := ParseObject([]byte(`[1,2,3]`))
	require.Error(t, err)
	_, err = ParseObject([]byte(`"just a string"`))
	require.Error(t, err)
}

func TestOrderedMap_RoundTripsNestedStructures(t *testing.T) {
	raw := []byte(`{"choices":[{"index":0,"delta":{"content":"hi"}}],"usage":{"total_tokens":5}}`)
	m, err := ParseObject(raw)
	require.NoError(t, err)
	out, err := m.MarshalJSON()
	require.NoError(t, err)

	var want, got interface{}
	require.NoError(t, json.Unmarshal(raw, &want))
	require.NoError(t, json.Unmarshal(out, &got))
	require.Equal(t, want, got)
}
