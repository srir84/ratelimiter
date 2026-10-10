// Package keys encodes budget keys for the store and builds the store key names (DESIGN
// §A3.2, §A4.1; SPEC API-1). A budget key is 1–256 bytes of UTF-8; its encoding enc is
// base64url without padding, so any key is safe in a key name and distinct keys never
// collide.
package keys
