package proxy

import (
	"crypto/rand"
	"encoding/hex"
)

// newRequestID generates the router's own opaque request identifier. It is
// substituted for whatever id the upstream returned, and is the value put
// in the X-Request-Id response header and chatcmpl .id field.
func newRequestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "chatcmpl-" + hex.EncodeToString(b[:])
}
