package interactive

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/RenseiAI/donmai/attachwire"
	"github.com/RenseiAI/donmai/attachwire/sanitize"
)

// TestFuzzCorpusRegression feeds the checked-in fuzz seed/regression corpora
// through the PUBLIC decode + sanitize surface and asserts the same invariants
// the in-package fuzz targets hold — panic-free decode, and canonical idempotence
// on anything that decodes cleanly. It is the platform-free regression guard that
// the adversarial corpus stays green against the exported API a downstream
// embedder actually links.
//
// The corpora live in the donmai module (resolved via the go.mod replace):
//
//	attachwire/testdata/fuzz/{FuzzDecodeFrame,FuzzDecodeControl,FuzzDecodeSnap}
//	attachwire/sanitize/testdata/fuzz/FuzzSanitizer
func TestFuzzCorpusRegression(t *testing.T) {
	moduleDir := donmaiModuleDir(t)
	awFuzz := filepath.Join(moduleDir, "attachwire", "testdata", "fuzz")
	sanFuzz := filepath.Join(moduleDir, "attachwire", "sanitize", "testdata", "fuzz")

	t.Run("FuzzDecodeFrame", func(t *testing.T) {
		corpus := readCorpusDir(t, filepath.Join(awFuzz, "FuzzDecodeFrame"))
		for name, data := range corpus {
			checkNoPanic(t, name, data, func() {
				fr, err := attachwire.DecodeFrame(data)
				if err != nil {
					return
				}
				re, err := attachwire.DecodeFrame(attachwire.EncodeFrame(fr))
				if err != nil {
					t.Errorf("%s: re-decode of a valid frame failed: %v", name, err)
					return
				}
				if re.Type != fr.Type || re.Seq != fr.Seq || re.RelTime != fr.RelTime || !bytes.Equal(re.Payload, fr.Payload) {
					t.Errorf("%s: frame not canonical-idempotent", name)
				}
			})
		}
		t.Logf("FuzzDecodeFrame: %d corpus entries, all panic-free", len(corpus))
	})

	t.Run("FuzzDecodeControl", func(t *testing.T) {
		corpus := readCorpusDir(t, filepath.Join(awFuzz, "FuzzDecodeControl"))
		for name, data := range corpus {
			checkNoPanic(t, name, data, func() {
				msg, err := attachwire.DecodeControl(data)
				if err != nil {
					return
				}
				raw, err := attachwire.MarshalControl(msg)
				if err != nil {
					t.Errorf("%s: re-marshal of a decoded control failed: %v", name, err)
					return
				}
				msg2, err := attachwire.DecodeControl(raw)
				if err != nil {
					t.Errorf("%s: re-decode of a re-marshaled control failed: %v", name, err)
					return
				}
				if msg.ControlType() != msg2.ControlType() {
					t.Errorf("%s: control type changed across round trip: %q -> %q", name, msg.ControlType(), msg2.ControlType())
				}
			})
		}
		t.Logf("FuzzDecodeControl: %d corpus entries, all panic-free", len(corpus))
	})

	t.Run("FuzzDecodeSnap", func(t *testing.T) {
		corpus := readCorpusDir(t, filepath.Join(awFuzz, "FuzzDecodeSnap"))
		for name, data := range corpus {
			checkNoPanic(t, name, data, func() {
				s, err := attachwire.DecodeScreen(data)
				if err != nil {
					return
				}
				enc, err := s.Encode()
				if err != nil {
					t.Errorf("%s: re-encode of a decoded screen failed: %v", name, err)
					return
				}
				s2, err := attachwire.DecodeScreen(enc)
				if err != nil {
					t.Errorf("%s: re-decode of a re-encoded screen failed: %v", name, err)
					return
				}
				if !reflect.DeepEqual(s, s2) {
					t.Errorf("%s: screen not canonical-idempotent", name)
				}
			})
		}
		t.Logf("FuzzDecodeSnap: %d corpus entries, all panic-free", len(corpus))
	})

	t.Run("FuzzSanitizer", func(t *testing.T) {
		corpus := readCorpusDir(t, filepath.Join(sanFuzz, "FuzzSanitizer"))
		for name, data := range corpus {
			checkNoPanic(t, name, data, func() {
				out := sanitize.New().Write(data)
				// (2) idempotence: re-sanitizing the output changes nothing.
				if again := sanitize.New().Write(out); !bytes.Equal(again, out) {
					t.Errorf("%s: sanitizer not idempotent", name)
				}
				// (3) chunked == contiguous for several fixed split widths — the
				// split-sequence bypass must stay closed.
				for _, w := range []int{1, 2, 3, 5, 7} {
					s := sanitize.New()
					var chunked []byte
					for pos := 0; pos < len(data); pos += w {
						end := pos + w
						if end > len(data) {
							end = len(data)
						}
						chunked = append(chunked, s.Write(data[pos:end])...)
					}
					if !bytes.Equal(chunked, out) {
						t.Errorf("%s: chunked(width=%d) != contiguous", name, w)
						break
					}
				}
			})
		}
		t.Logf("FuzzSanitizer: %d corpus entries, all panic-free", len(corpus))
	})
}

// checkNoPanic runs fn and converts a panic into a clean test failure that names
// the offending corpus entry (the fuzz invariant: decoders never panic).
func checkNoPanic(t *testing.T, name string, data []byte, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("PANIC on corpus %q (%q): %v", name, fmt.Sprintf("%x", data), r)
		}
	}()
	fn()
}
