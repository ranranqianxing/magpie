package cursoride

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

func TestDecodeCatalogBodyGunzipsBeforeMerge(t *testing.T) {
	extra := AvailableModels([]Model{{ID: "deepseek/deepseek-flash", Name: "DeepSeek-V4.1-Flash", Context: 200000}})
	plainUp := AvailableModels([]Model{{ID: "claude-4.5", Name: "Claude", Context: 200000}})
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plainUp); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	gz := buf.Bytes()
	decoded, err := decodeCatalogBody(gz, "gzip")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(decoded, gz) {
		t.Fatal("still gzip")
	}
	merged, err := MergeUnary(decoded, extra)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(merged, []byte("deepseek/deepseek-flash")) {
		t.Fatal("magpie model missing after merge")
	}
	if !bytes.Contains(merged, []byte("claude-4.5")) {
		t.Fatal("upstream model missing after merge")
	}
	// Naive merge leaves trailing bytes after the gzip member; a full
	// decompress of that blob fails (what Cursor effectively sees).
	bad, err := MergeUnary(gz, extra)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(bad))
	if err != nil {
		return // already unreadable as gzip
	}
	_, err = io.ReadAll(zr)
	_ = zr.Close()
	if err == nil && bytes.Contains(bad[len(gz):], []byte("deepseek/")) {
		// Reader stopped at member end; magpie bytes were never protobuf-parsed.
		t.Fatal("naive merge hides magpie models after gzip member")
	}
}

func TestDecodeCatalogBodyPlainPassthrough(t *testing.T) {
	plain := []byte{0x0a, 0x05, 'h', 'e', 'l', 'l', 'o'}
	out, err := decodeCatalogBody(plain, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, plain) {
		t.Fatalf("%q", out)
	}
}
