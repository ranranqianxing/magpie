package cursoride

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// maxImageFile is how much of a selected image path is read. A larger
// file is left out. The bytes are not logged.
const maxImageFile = 8 << 20

// userAttach is context the current UserMessage carried beside the typed
// text. Cursor's own agent renders selected_context into the prompt.
// The typed text alone is what was left when this was dropped.
//
// pieces are file text, code selections, extra context and attached
// rules, not yet joined. They are filtered against the turn they land
// on, so a history message that already contains the file is not given
// the same bytes again. images are data URLs. missing counts image blob
// ids this request did not carry bytes for. skipped is a selected_context
// that did not parse: the typed text is kept, and the attachment is
// unknown rather than empty by invention.
type attachPiece struct {
	path, rel, body, rendered string
}

type userAttach struct {
	pieces  []attachPiece
	images  []ContentPart
	missing int
	skipped bool
}

func (a userAttach) empty() bool {
	return len(a.pieces) == 0 && len(a.images) == 0
}

// noteFor joins pieces that are not already in text. body is the raw
// file or rule text; rendered is what the model reads, including the label.
func (a userAttach) noteFor(text string) string {
	var kept []string
	seen := text
	for _, p := range a.pieces {
		rendered := strings.TrimSpace(p.rendered)
		if rendered == "" || strings.Contains(seen, rendered) || alreadyIn(seen, p.path, p.rel, p.body) {
			continue
		}
		kept = append(kept, rendered)
		seen += "\n" + rendered
	}
	return strings.Join(kept, "\n\n")
}

// readUserAttach reads UserMessage.selected_context (field 3), or field 10
// when that blob is the context. Field numbers are agent.v1.UserMessage
// and SelectedContext. A context that does not parse does not drop the
// typed text the caller already has.
func readUserAttach(run []field, prefetched map[string][]byte) userAttach {
	msg, ok := readUserMessage(run)
	if !ok {
		return userAttach{}
	}
	raw, skipped := contextBytes(msg, prefetched)
	if skipped {
		return userAttach{skipped: true, missing: 1}
	}
	if len(raw) == 0 {
		return userAttach{}
	}
	fs, err := parseFields(raw)
	if err != nil {
		return userAttach{skipped: true}
	}
	return attachFromContext(fs, prefetched)
}

func contextBytes(msg []field, prefetched map[string][]byte) (raw []byte, skipped bool) {
	if blobs := fieldBytes(msg, 3); len(blobs) > 0 {
		return blobs[0], false
	}
	blobs := fieldBytes(msg, 10)
	if len(blobs) == 0 {
		return nil, false
	}
	b := blobs[0]
	// A 32-byte value is a blob id, not a SelectedContext.
	if len(b) == 32 {
		body, miss := resolveBlob(b, prefetched)
		if miss > 0 || len(body) == 0 {
			return nil, true
		}
		return body, false
	}
	if fs, err := parseFields(b); err == nil && contextHasPieces(fs) {
		return b, false
	}
	body, miss := resolveBlob(b, prefetched)
	if miss > 0 || len(body) == 0 {
		return nil, true
	}
	return body, false
}

func contextHasPieces(fs []field) bool {
	for _, n := range []int{1, 3, 4, 5, 10, 16} {
		if hasField(fs, n) {
			return true
		}
	}
	return false
}

func attachFromContext(fs []field, prefetched map[string][]byte) userAttach {
	var a userAttach
	for _, raw := range fieldBytes(fs, 3) {
		if s := strings.TrimSpace(string(raw)); s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
			a.pieces = append(a.pieces, attachPiece{body: s, rendered: s})
		}
	}
	for _, raw := range fieldBytes(fs, 16) {
		if s := entryText(raw, prefetched); s != "" {
			a.pieces = append(a.pieces, attachPiece{body: s, rendered: s})
		}
	}
	var files []namedText
	for _, raw := range fieldBytes(fs, 4) {
		if f, ok := namedFrom(raw); ok {
			files = append(files, f)
			if s := fileNote(f); s != "" {
				a.pieces = append(a.pieces, attachPiece{path: f.path, rel: f.rel, body: f.content, rendered: s})
			}
		}
	}
	for _, raw := range fieldBytes(fs, 5) {
		if p, ok := selectionPiece(raw, files); ok {
			a.pieces = append(a.pieces, p)
		}
	}
	for _, raw := range fieldBytes(fs, 10) {
		if p, ok := rulePiece(raw); ok {
			a.pieces = append(a.pieces, p)
		}
	}
	for _, raw := range fieldBytes(fs, 1) {
		part, miss := imageFromSelected(raw, prefetched)
		a.missing += miss
		if part.Type != "" && !hasImage(a.images, part.ImageURL) {
			a.images = append(a.images, part)
		}
	}
	return a
}

type namedText struct {
	path, rel, content string
}

func namedFrom(raw []byte) (namedText, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return namedText{}, false
	}
	path, _ := fieldString(fs, 2)
	rel, _ := fieldString(fs, 3)
	content, _ := fieldString(fs, 1)
	if strings.ContainsRune(content, 0) {
		content = ""
	}
	if path == "" && rel == "" && content == "" {
		return namedText{}, false
	}
	return namedText{path: path, rel: rel, content: content}, true
}

func alreadyIn(text, path, rel, content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		label := pathLabel(path, rel)
		return label != "" && strings.Contains(text, label)
	}
	if !strings.Contains(text, content) {
		return false
	}
	if path == "" && rel == "" {
		return len(content) >= 24
	}
	return strings.Contains(text, path) || (rel != "" && strings.Contains(text, rel))
}

func fileNote(f namedText) string {
	label := pathLabel(f.path, f.rel)
	if f.content == "" {
		if label == "" {
			return ""
		}
		return "Attached file " + label
	}
	if label == "" {
		return "Attached file:\n" + f.content
	}
	return "Attached file " + label + ":\n" + f.content
}

func selectionPiece(raw []byte, files []namedText) (attachPiece, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return attachPiece{}, false
	}
	path, _ := fieldString(fs, 2)
	rel, _ := fieldString(fs, 3)
	content, _ := fieldString(fs, 1)
	if redundantSelection(content, path, rel, files) {
		return attachPiece{}, false
	}
	label := pathLabel(path, rel)
	head := "Code selection"
	if label != "" {
		head += " " + label
	}
	if lines := rangeLabel(fieldBytes(fs, 4)); lines != "" {
		head += " " + lines
	}
	rendered := head
	if content != "" {
		rendered += ":\n" + content
	}
	if path == "" && rel == "" && content == "" {
		return attachPiece{}, false
	}
	return attachPiece{path: path, rel: rel, body: content, rendered: rendered}, true
}

func redundantSelection(content, path, rel string, files []namedText) bool {
	if content == "" {
		return false
	}
	for _, f := range files {
		if f.content == "" || !strings.Contains(f.content, content) {
			continue
		}
		if path == "" && rel == "" {
			return true
		}
		if path != "" && (path == f.path || path == f.rel) {
			return true
		}
		if rel != "" && (rel == f.path || rel == f.rel) {
			return true
		}
	}
	return false
}

func rulePiece(raw []byte) (attachPiece, bool) {
	fs, err := parseFields(raw)
	if err != nil {
		return attachPiece{}, false
	}
	rules := fieldBytes(fs, 1)
	if len(rules) == 0 {
		return attachPiece{}, false
	}
	rf, err := parseFields(rules[0])
	if err != nil {
		return attachPiece{}, false
	}
	path, _ := fieldString(rf, 1)
	content, _ := fieldString(rf, 2)
	if path == "" && strings.TrimSpace(content) == "" {
		return attachPiece{}, false
	}
	rendered := "Attached rule " + path
	if strings.TrimSpace(content) != "" {
		if path == "" {
			rendered = "Attached rule:\n" + content
		} else {
			rendered = "Attached rule " + path + ":\n" + content
		}
	}
	return attachPiece{path: path, body: content, rendered: rendered}, true
}

func entryText(raw []byte, prefetched map[string][]byte) string {
	fs, err := parseFields(raw)
	if err != nil {
		return ""
	}
	if s, ok := fieldString(fs, 1); ok {
		s = strings.TrimSpace(s)
		if s != "" && !strings.ContainsRune(s, 0) {
			return s
		}
	}
	ids := fieldBytes(fs, 2)
	if len(ids) == 0 {
		return ""
	}
	body, miss := resolveBlob(ids[0], prefetched)
	if miss > 0 || len(body) == 0 || !utf8.Valid(body) || strings.ContainsRune(string(body), 0) {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func pathLabel(path, rel string) string {
	path = strings.TrimSpace(path)
	rel = strings.TrimSpace(rel)
	switch {
	case rel != "" && path != "" && rel != path:
		return rel + " (" + path + ")"
	case path != "":
		return path
	default:
		return rel
	}
}

func rangeLabel(raws [][]byte) string {
	if len(raws) == 0 {
		return ""
	}
	fs, err := parseFields(raws[0])
	if err != nil {
		return ""
	}
	start, sok := positionLine(fieldBytes(fs, 1))
	end, eok := positionLine(fieldBytes(fs, 2))
	switch {
	case sok && eok && start != end:
		return "lines " + uitoa(start) + "-" + uitoa(end)
	case sok:
		return "line " + uitoa(start)
	case eok:
		return "line " + uitoa(end)
	default:
		return ""
	}
}

func positionLine(raws [][]byte) (uint64, bool) {
	if len(raws) == 0 {
		return 0, false
	}
	fs, err := parseFields(raws[0])
	if err != nil {
		return 0, false
	}
	return fieldUint(fs, 1)
}

func imageFromSelected(raw []byte, prefetched map[string][]byte) (ContentPart, int) {
	fs, err := parseFields(raw)
	if err != nil {
		return ContentPart{}, 1
	}
	mime, _ := fieldString(fs, 7)
	if data := fieldBytes(fs, 8); len(data) > 0 && len(data[0]) > 0 {
		if part, ok := imageBytesPart(data[0], mime); ok {
			return part, 0
		}
	}
	if wrapped := fieldBytes(fs, 9); len(wrapped) > 0 {
		wf, err := parseFields(wrapped[0])
		if err != nil {
			return ContentPart{}, 1
		}
		if data := fieldBytes(wf, 2); len(data) > 0 && len(data[0]) > 0 {
			if part, ok := imageBytesPart(data[0], mime); ok {
				return part, 0
			}
		}
		if ids := fieldBytes(wf, 1); len(ids) > 0 {
			part, miss := imageBlob(ids[0], mime, prefetched)
			if part.Type != "" {
				return part, 0
			}
			if path, ok := fieldString(fs, 3); ok {
				if part, ok := imageFromPath(path, mime); ok {
					return part, 0
				}
			}
			return ContentPart{}, miss
		}
	}
	if ids := fieldBytes(fs, 1); len(ids) > 0 {
		part, miss := imageBlob(ids[0], mime, prefetched)
		if part.Type != "" {
			return part, 0
		}
		if path, ok := fieldString(fs, 3); ok {
			if part, ok := imageFromPath(path, mime); ok {
				return part, 0
			}
		}
		return ContentPart{}, miss
	}
	if path, ok := fieldString(fs, 3); ok && strings.TrimSpace(path) != "" {
		part, ok := imageFromPath(path, mime)
		if ok {
			return part, 0
		}
		return ContentPart{}, 1
	}
	return ContentPart{}, 0
}

// imageFromPath reads a selected image Cursor named by path and did not
// inline. The proxy runs on the same machine as the IDE. Only a regular
// file that sniffs as an image and fits in maxImageFile is attached.
// The path is not logged.
func imageFromPath(path, mime string) (ContentPart, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return ContentPart{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		Note("agent image skipped reason=path")
		return ContentPart{}, false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		Note("agent image skipped reason=path")
		return ContentPart{}, false
	}
	if st.Size() <= 0 || st.Size() > maxImageFile {
		Note("agent image skipped reason=too-large")
		return ContentPart{}, false
	}
	buf := make([]byte, st.Size())
	if _, err := io.ReadFull(f, buf); err != nil {
		Note("agent image skipped reason=path")
		return ContentPart{}, false
	}
	part, ok := imageBytesPart(buf, mime)
	if !ok {
		Note("agent image skipped reason=not-image")
		return ContentPart{}, false
	}
	Note(fmt.Sprintf("agent image source=path bytes=%d mime=%s", len(buf), sniffedOr(part.ImageURL, mime)))
	return part, true
}

func sniffedOr(dataURL, mime string) string {
	if i := strings.Index(dataURL, ":"); i >= 0 {
		rest := dataURL[i+1:]
		if j := strings.Index(rest, ";"); j > 0 {
			return rest[:j]
		}
	}
	if mime != "" {
		return mime
	}
	return "image"
}

func imageBlob(id []byte, mime string, prefetched map[string][]byte) (ContentPart, int) {
	body, miss := resolveBlob(id, prefetched)
	if miss > 0 || len(body) == 0 {
		return ContentPart{}, 1
	}
	part, ok := imageBytesPart(body, mime)
	if !ok {
		return ContentPart{}, 0
	}
	return part, 0
}

func imageBytesPart(data []byte, mime string) (ContentPart, bool) {
	if len(data) == 0 {
		return ContentPart{}, false
	}
	if sniff := sniffImageMime(data); sniff != "" {
		if !strings.HasPrefix(strings.ToLower(mime), "image/") {
			mime = sniff
		}
		return imagePart(base64.StdEncoding.EncodeToString(data), mime)
	}
	if utf8.Valid(data) {
		s := strings.TrimSpace(string(data))
		if strings.HasPrefix(s, "data:image") {
			return ContentPart{Type: "image_url", ImageURL: s}, true
		}
		if payload, sniff, ok := base64Image(s); ok {
			if !strings.HasPrefix(strings.ToLower(mime), "image/") {
				mime = sniff
			}
			return imagePart(payload, mime)
		}
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") {
		return ContentPart{}, false
	}
	return imagePart(base64.StdEncoding.EncodeToString(data), mime)
}

func imagePart(payload, mime string) (ContentPart, bool) {
	if mime == "" {
		mime = "image/png"
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") {
		return ContentPart{}, false
	}
	return ContentPart{Type: "image_url", ImageURL: imageDataURL(payload, mime)}, true
}

// base64Image reports whether s is already the base64 of an image, so a
// selected image stored as text is not encoded a second time.
func base64Image(s string) (payload, mime string, ok bool) {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, s)
	if len(s) < 8 || strings.ContainsAny(s, "{}") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(s)
		if err != nil {
			return "", "", false
		}
	}
	mime = sniffImageMime(decoded)
	if mime == "" {
		return "", "", false
	}
	return base64.StdEncoding.EncodeToString(decoded), mime, true
}

func sniffImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G':
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

func uitoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func joinNote(text, note string) string {
	note = strings.TrimSpace(note)
	if note == "" || strings.Contains(text, note) {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return note
	}
	return text + "\n\n" + note
}
