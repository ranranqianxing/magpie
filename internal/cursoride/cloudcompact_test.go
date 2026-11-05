package cursoride

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// officialRow is one Cursor catalog row that does not set
// visible_in_routed_model_view. defaultOn and a named section are what
// the named picker shows before the routed compact list takes over.
func officialRow(id, name string, edit func(*wbuf)) []byte {
	var b wbuf
	b.str(1, id)
	b.bool(2, true)
	b.str(17, name)
	b.u32(38, 0)
	if edit != nil {
		edit(&b)
	}
	return []byte(b)
}

func catalogMessage(rows ...[]byte) []byte {
	var b wbuf
	for _, row := range rows {
		fs, err := parseFields(row)
		if err != nil {
			panic(err)
		}
		id, _ := fieldString(fs, 1)
		b.str(1, id)
	}
	for _, row := range rows {
		b.msg(2, row)
	}
	// A sibling the cloud window still reads. Promotion must leave it.
	b.str(19, "display-kept")
	return []byte(b)
}

func cloudEffortRequest() []byte {
	var b wbuf
	b.bool(3, true)
	b.bool(5, true)
	b.bool(11, true)
	b.bool(12, true)
	return frameOf([]byte(b))
}

func ideCatalogRequest() []byte {
	var b wbuf
	b.bool(3, true)
	b.bool(5, true)
	b.bool(11, true)
	return frameOf([]byte(b))
}

func routedOn(t *testing.T, row []byte) bool {
	t.Helper()
	fs, err := parseFields(row)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := lastVarint(fs, fieldRoutedView)
	return ok && v == 1
}

func rowByName(t *testing.T, payload, id string) []byte {
	t.Helper()
	fs, err := parseFields([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range fieldBytes(fs, 2) {
		row, err := parseFields(raw)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := fieldString(row, 1)
		if got == id {
			return raw
		}
	}
	t.Fatalf("no row %s", id)
	return nil
}

func TestCloudCompactKeepsOfficialRowsBesideMagpie(t *testing.T) {
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	composer := officialRow("composer-2.5", "Composer 2.5", func(b *wbuf) {
		var vendor wbuf
		vendor.u32(1, modelVendorCursor)
		vendor.str(2, "cursor")
		b.msg(42, vendor)
	})
	// Show-all row: no default_on, so the compact list never had it.
	hiddenFromCompact := func() []byte {
		var b wbuf
		b.str(1, "claude-opus-5")
		b.bool(2, false)
		b.str(17, "Opus")
		b.u32(38, 2)
		return []byte(b)
	}()
	upstream := frameOf(catalogMessage(grok, composer, hiddenFromCompact))
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(_ *http.Request, body []byte) (int, []byte, string, error) {
			if !cloudCatalogRequest(body) {
				t.Fatal("upstream was not the cloud catalog call")
			}
			return 200, upstream, "application/grpc", nil
		},
	}
	rec := post(t, h, PathAvailableModels, cloudEffortRequest())
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	payload, framed, err := UnwrapUnary(rec.Body.Bytes())
	if err != nil || !framed {
		t.Fatalf("framed=%v err=%v", framed, err)
	}
	if !strings.Contains(string(payload), "display-kept") {
		t.Fatal("cloud catalog lost a sibling field")
	}
	names := fieldStrings(mustParse(t, string(payload)), 1)
	if len(names) != 4 || names[0] != "grok-4.7" || names[1] != "composer-2.5" || names[2] != "claude-opus-5" || names[3] != "oppo/deepseek-flash" {
		t.Fatalf("names = %q", names)
	}
	if !routedOn(t, rowByName(t, string(payload), "grok-4.7")) || !routedOn(t, rowByName(t, string(payload), "composer-2.5")) {
		t.Fatal("official cloud rows stayed out of the compact list")
	}
	if routedOn(t, rowByName(t, string(payload), "claude-opus-5")) {
		t.Fatal("a show-all row was pulled into the compact list")
	}
	if !routedOn(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("magpie row lost visible_in_routed_model_view")
	}
}

func TestIDECatalogDoesNotStampRoutedOnOfficialRows(t *testing.T) {
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	upstream := frameOf(catalogMessage(grok))
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, upstream, "application/proto", nil
		},
	}
	rec := post(t, h, PathAvailableModels, ideCatalogRequest())
	payload, _, err := UnwrapUnary(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	row := rowByName(t, string(payload), "grok-4.7")
	if routedOn(t, row) {
		t.Fatal("IDE catalog gained visible_in_routed_model_view on an official row")
	}
	if !bytes.Contains(payload, grok) {
		t.Fatal("IDE merge rewrote the official row")
	}
	if !routedOn(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("magpie row was not routed on the IDE catalog")
	}
}

func TestCloudCompactLeavesOutRowsThePickerDoesNotShow(t *testing.T) {
	cases := []struct {
		name string
		row  []byte
	}{
		{"auto", officialRow("default", "Auto", nil)},
		{"hidden", officialRow("grok-4.7", "Grok", func(b *wbuf) { b.bool(35, true) })},
		{"migrating", officialRow("grok-4.7", "Grok", func(b *wbuf) { b.str(33, "grok-4.6") })},
		{"cmd-k", officialRow("grok-4.7", "Grok", func(b *wbuf) { b.bool(27, true) })},
		{"disabled", officialRow("grok-4.7", "Grok", func(b *wbuf) { b.u32(6, degradationDisabled) })},
		{"already", officialRow("grok-4.7", "Grok", func(b *wbuf) { b.bool(40, true) })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := catalogMessage(tc.row)
			out := promoteCloudCompact(in)
			if tc.name == "already" {
				if !bytes.Equal(out, in) {
					t.Fatal("rewrote a row that was already in the compact list")
				}
				return
			}
			if routedOn(t, rowByName(t, string(out), fieldName(t, tc.row))) && tc.name != "already" {
				t.Fatalf("%s row was marked visible in the routed view", tc.name)
			}
		})
	}
}

func TestCloudCompactPromotesCloudEffortWithoutASection(t *testing.T) {
	var effort wbuf
	effort.str(1, "grok-4.7")
	effort.bool(2, true)
	effort.str(17, "Grok 4.7")
	effort.u32(32, 1)
	var off wbuf
	off.str(1, "composer-2.5")
	off.bool(2, true)
	off.str(17, "Composer 2.5")
	off.bool(40, false)
	off.u32(38, 0)
	off.fixed64(7, 0x3ff0000000000000)
	got := promoteCloudCompact(catalogMessage([]byte(effort), []byte(off)))
	if !routedOn(t, rowByName(t, string(got), "grok-4.7")) {
		t.Fatal("cloud-effort row stayed out of the compact list")
	}
	composer := rowByName(t, string(got), "composer-2.5")
	if !routedOn(t, composer) {
		t.Fatal("explicit false stayed out of the compact list")
	}
	for _, f := range mustParse(t, string(composer)) {
		if f.num == fieldRoutedView && f.v == 0 {
			t.Fatal("visible_in_routed_model_view false was left in place")
		}
	}
	// Field 7, wire type 1, then the little-endian price bits.
	price := []byte{0x39, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xf0, 0x3f}
	if !bytes.Contains(composer, price) {
		t.Fatalf("price bytes dropped: %x", composer)
	}
}

func TestGzippedCloudRequestStillPromotes(t *testing.T) {
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	if _, err := zw.Write(cloudEffortRequest()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	h := &Handler{
		Models: func() []Model { return nil },
		Upstream: func(_ *http.Request, body []byte) (int, []byte, string, error) {
			if cloudCatalogRequest(body) {
				t.Fatal("gzipped request was parsed without decompressing")
			}
			return 200, catalogMessage(grok), "application/proto", nil
		},
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, PathAvailableModels, bytes.NewReader(z.Bytes()))
	req.Header.Set("Content-Encoding", "gzip")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.Bytes())
	}
	if !routedOn(t, rowByName(t, rec.Body.String(), "grok-4.7")) {
		t.Fatal("gzipped cloud request left the official row unrouted")
	}
}

func TestAutomationsRequestDoesNotPromote(t *testing.T) {
	var req wbuf
	req.bool(5, true)
	req.bool(9, true) // for_automations
	req.u32(10, 2)    // AVAILABLE_MODELS_SCOPE_AUTOMATIONS
	req.bool(15, true)
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, catalogMessage(grok), "application/proto", nil
		},
	}
	rec := post(t, h, PathAvailableModels, frameOf([]byte(req)))
	payload, _, err := UnwrapUnary(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if routedOn(t, rowByName(t, string(payload), "grok-4.7")) {
		t.Fatal("automations catalog stamped visible_in_routed_model_view onto an official row")
	}
	if !routedOn(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("magpie row was dropped from the automations catalog")
	}
}

func mustParse(t *testing.T, payload string) []field {
	t.Helper()
	fs, err := parseFields([]byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func fieldName(t *testing.T, row []byte) string {
	t.Helper()
	fs, err := parseFields(row)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := fieldString(fs, 1)
	return id
}
