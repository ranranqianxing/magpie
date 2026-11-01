package cursoride

import (
	"strings"
	"testing"
)

func TestPiResultFeedsErrorPatchAndTruncation(t *testing.T) {
	var errMsg wbuf
	errMsg.str(1, "old-text-absent")
	var result wbuf
	result.msg(2, []byte(errMsg))
	got := execToolResults(piResultClient(1, "call_e", 48, []byte(result)), map[string]bool{"call_e": true}, nil)
	if len(got) != 1 || got[0].Content.(string) != "error: old-text-absent" {
		t.Fatalf("edit error %+v", got)
	}
	if !strings.Contains(Recent(), "kind=pi-edit") || !strings.Contains(Recent(), "status=error") || strings.Contains(Recent(), "old-text-absent") {
		t.Fatalf("error trace leaked text or was missing:\n%s", Recent())
	}

	var success wbuf
	success.str(1, "edited")
	success.str(2, "-old\n+new")
	success.str(3, "@@ patch")
	success.u32(4, 12)
	var okResult wbuf
	okResult.msg(1, []byte(success))
	got = execToolResults(piResultClient(1, "call_e", 48, []byte(okResult)), map[string]bool{"call_e": true}, nil)
	text, _ := got[0].Content.(string)
	if !strings.Contains(text, "edited") || !strings.Contains(text, "-old") || !strings.Contains(text, "@@ patch") || !strings.Contains(text, "first_changed_line=12") {
		t.Fatalf("edit success %q", text)
	}

	var trunc wbuf
	trunc.u32(1, 1)
	trunc.str(2, "bytes")
	trunc.u32(3, 80)
	trunc.u32(4, 10)
	var readOK wbuf
	readOK.str(1, "snippet-alpha")
	readOK.msg(2, []byte(trunc))
	var readResult wbuf
	readResult.msg(1, []byte(readOK))
	got = execToolResults(piResultClient(1, "call_r", 46, []byte(readResult)), map[string]bool{"call_r": true}, nil)
	text, _ = got[0].Content.(string)
	if !strings.Contains(text, "snippet-alpha") || !strings.Contains(text, "[truncated by=bytes total_lines=80 output_lines=10]") {
		t.Fatalf("read truncation %q", text)
	}
	if !strings.Contains(Recent(), "kind=pi-read") || !strings.Contains(Recent(), "truncated=true") || strings.Contains(Recent(), "snippet-alpha") {
		t.Fatalf("truncation trace:\n%s", Recent())
	}

	var empty wbuf
	got = execToolResults(piResultClient(1, "call_r", 46, []byte(empty)), map[string]bool{"call_r": true}, nil)
	if len(got) != 1 || got[0].Content.(string) != "error: tool result was empty" {
		t.Fatalf("empty oneof %+v", got)
	}
}

func piResultClient(n uint32, callID string, resultField int, result []byte) []byte {
	var exec wbuf
	exec.u32(1, n)
	exec.str(15, callID)
	exec.msg(resultField, result)
	var client wbuf
	client.msg(2, []byte(exec))
	return []byte(client)
}
