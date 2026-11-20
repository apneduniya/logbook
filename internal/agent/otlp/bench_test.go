package otlp

import (
	"net/http"
	"testing"
)

// One export per minute must cost next to nothing: decoding plus storing a
// realistic Claude Code metrics request.
func BenchmarkExport(b *testing.B) {
	body := fixture(b, "delta.json")
	e := newEnv(b)
	b.ReportAllocs()
	for b.Loop() {
		if rec := e.post(b, "/v1/metrics", body); rec.Code != http.StatusOK {
			b.Fatalf("status %d", rec.Code)
		}
	}
}

func BenchmarkDecode(b *testing.B) {
	body := fixture(b, "delta.json")
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Decode(body); err != nil {
			b.Fatal(err)
		}
	}
}
