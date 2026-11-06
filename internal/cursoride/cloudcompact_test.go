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
	row := rowByName(t, string(payload), "oppo/deepseek-flash")
	if routedPresent(t, row) {
		t.Fatal("automations catalog set visible_in_routed_model_view on a magpie row")
	}
	if !defaultOn(t, row) {
		t.Fatal("automations catalog cleared default_on")
	}
}

// TestAutomationsKeepsMagpieBesideOfficialRows is the call Cursor's
// Automations picker actually sends: use_model_parameters,
// use_parameterized_automations_models, and scope AUTOMATIONS. It does
// not set for_automations. The client then drops a row that has
// visible_in_routed_model_view. The compact lists are other calls and
// still get the flag.
func TestAutomationsKeepsMagpieBesideOfficialRows(t *testing.T) {
	var req wbuf
	req.bool(5, true)
	req.u32(10, scopeAutomations)
	req.bool(15, true)
	plain := officialRow("grok-4.7", "Grok 4.7", nil)
	marked := officialRow("composer-2.5", "Composer 2.5", func(b *wbuf) { b.bool(40, true) })
	h := &Handler{
		Models: func() []Model { return []Model{{ID: "oppo/deepseek-flash", Name: "Grok 4.7", Context: 128000}} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, catalogMessage(plain, marked), "application/proto", nil
		},
	}
	rec := post(t, h, PathAvailableModels, frameOf([]byte(req)))
	payload := catalogPayload(t, rec)
	if !bytes.Contains(payload, plain) || !bytes.Contains(payload, marked) {
		t.Fatal("automations merge rewrote an official row")
	}
	if routedOn(t, rowByName(t, string(payload), "grok-4.7")) {
		t.Fatal("an official row that omitted the flag gained it")
	}
	if !routedOn(t, rowByName(t, string(payload), "composer-2.5")) {
		t.Fatal("an official row Cursor marked routed was cleared")
	}
	row := rowByName(t, string(payload), "oppo/deepseek-flash")
	if routedPresent(t, row) {
		t.Fatal("magpie row would be dropped by excludeRoutedModels")
	}
	fs := mustParse(t, string(row))
	if got, _ := fieldString(fs, 1); got != "oppo/deepseek-flash" {
		t.Fatalf("route id = %q", got)
	}
	if got, _ := fieldString(fs, 18); got != "oppo/deepseek-flash" {
		t.Fatalf("server_model_name = %q", got)
	}
	if got, _ := fieldString(fs, 17); got != "Grok 4.7 · Magpie" {
		t.Fatalf("display = %q", got)
	}
}

func TestAutomationsSignalsEachOmitTheRoutedFlag(t *testing.T) {
	cases := []struct {
		name string
		edit func(*wbuf)
	}{
		{"for-automations", func(b *wbuf) { b.bool(fieldForAutomations, true) }},
		{"scope", func(b *wbuf) { b.u32(fieldModelsScope, scopeAutomations) }},
		{"parameterized", func(b *wbuf) { b.bool(fieldParameterizedAutomations, true) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req wbuf
			req.bool(5, true)
			tc.edit(&req)
			payload := catalogReply(t, frameOf([]byte(req)), officialRow("grok-4.7", "Grok 4.7", nil))
			if routedPresent(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
				t.Fatal("magpie row kept visible_in_routed_model_view")
			}
			if routedOn(t, rowByName(t, string(payload), "grok-4.7")) {
				t.Fatal("official row gained visible_in_routed_model_view")
			}
		})
	}
}

func TestOtherScopesKeepTheRoutedFlag(t *testing.T) {
	cases := []struct {
		name  string
		scope uint32
	}{
		{"unspecified", 0},
		{"user", 1},
		{"admin", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req wbuf
			req.bool(5, true)
			req.bool(9, false)
			req.u32(10, tc.scope)
			req.bool(15, false)
			payload := catalogReply(t, frameOf([]byte(req)), officialRow("grok-4.7", "Grok 4.7", nil))
			if !routedOn(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
				t.Fatal("compact catalog lost visible_in_routed_model_view")
			}
			if routedOn(t, rowByName(t, string(payload), "grok-4.7")) {
				t.Fatal("a non-cloud catalog stamped an official row")
			}
		})
	}
}

func TestAutomationsPlusCloudDoesNotPromote(t *testing.T) {
	var req wbuf
	req.bool(12, true)
	req.u32(10, scopeAutomations)
	req.bool(15, true)
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	payload := catalogReply(t, frameOf([]byte(req)), grok)
	if got := rowByName(t, string(payload), "grok-4.7"); !bytes.Equal(got, grok) {
		t.Fatal("automations call rewrote the official row because the cloud flag was also set")
	}
	if routedPresent(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("automations call set the routed flag because the cloud flag was also set")
	}
}

func TestGzippedAutomationsRequestOmitsRoutedFlag(t *testing.T) {
	var req wbuf
	req.bool(5, true)
	req.u32(10, scopeAutomations)
	req.bool(15, true)
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	if _, err := zw.Write(frameOf([]byte(req))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	grok := officialRow("grok-4.7", "Grok 4.7", nil)
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(_ *http.Request, body []byte) (int, []byte, string, error) {
			if automationsCatalogRequest(body) {
				t.Fatal("gzipped automations request was parsed without decompressing")
			}
			return 200, catalogMessage(grok), "application/proto", nil
		},
	}
	rec := httptest.NewRecorder()
	httpReq := httptest.NewRequest(http.MethodPost, PathAvailableModels, bytes.NewReader(z.Bytes()))
	httpReq.Header.Set("Content-Encoding", "gzip")
	h.ServeHTTP(rec, httpReq)
	payload := catalogPayload(t, rec)
	if routedPresent(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("gzipped automations request kept the routed flag")
	}
	if routedOn(t, rowByName(t, string(payload), "grok-4.7")) {
		t.Fatal("gzipped automations request stamped an official row")
	}
}

func TestAutomationsUpstreamFailOmitsRoutedFlag(t *testing.T) {
	var req wbuf
	req.u32(10, scopeAutomations)
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 503, []byte("down"), "", nil
		},
	}
	rec := post(t, h, PathAvailableModels, frameOf([]byte(req)))
	payload := catalogPayload(t, rec)
	row := rowByName(t, string(payload), "oppo/deepseek-flash")
	if routedPresent(t, row) {
		t.Fatal("a failed automations upstream still set the routed flag")
	}
	if got, _ := fieldString(mustParse(t, string(row)), 1); got != "oppo/deepseek-flash" {
		t.Fatalf("route id = %q", got)
	}
}

func TestUnreadableCatalogRequestKeepsRoutedFlag(t *testing.T) {
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, catalogMessage(officialRow("grok-4.7", "Grok 4.7", nil)), "application/proto", nil
		},
	}
	// Not a protobuf message. A failed read is not an Automations call.
	rec := post(t, h, PathAvailableModels, []byte{0xff, 0xff, 0xff, 0xff, 0x0f})
	payload := catalogPayload(t, rec)
	if !routedOn(t, rowByName(t, string(payload), "oppo/deepseek-flash")) {
		t.Fatal("an unreadable catalog request dropped the routed flag")
	}
}

func catalogReply(t *testing.T, req, official []byte) []byte {
	t.Helper()
	h := &Handler{
		Models: func() []Model { return []Model{deepseek()} },
		Upstream: func(*http.Request, []byte) (int, []byte, string, error) {
			return 200, catalogMessage(official), "application/proto", nil
		},
	}
	return catalogPayload(t, post(t, h, PathAvailableModels, req))
}

func catalogPayload(t *testing.T, rec *httptest.ResponseRecorder) []byte {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("status %d %s", rec.Code, rec.Body.Bytes())
	}
	payload, _, err := UnwrapUnary(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func routedPresent(t *testing.T, row []byte) bool {
	t.Helper()
	fs, err := parseFields(row)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := lastVarint(fs, fieldRoutedView)
	return ok
}

func defaultOn(t *testing.T, row []byte) bool {
	t.Helper()
	fs, err := parseFields(row)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := lastVarint(fs, fieldDefaultOn)
	return ok && v == 1
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
