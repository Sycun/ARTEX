package enterprise

import (
	"strings"
	"testing"
)

func TestSanitizeRemovesFictionalPassword(t *testing.T) {
	in := "原件申请已拒绝，理由中却含同一个虚构口令 password=" + DemoSecret
	got := Sanitize(in)
	if !got.Removed {
		t.Fatal("expected credential removal")
	}
	if strings.Contains(got.Display, DemoSecret) || strings.Contains(got.Display, "password=") {
		t.Fatalf("display leaked credential: %s", got.Display)
	}
	if !strings.Contains(got.Display, redactedMark) {
		t.Fatalf("display missing redaction mark: %s", got.Display)
	}
	if got.Sealed != in {
		t.Fatal("sealed original was not retained")
	}
	plain := Sanitize("演示报告以脱敏正文为准，不查看原件。")
	if plain.Removed || strings.Contains(plain.Display, redactedMark) {
		t.Fatal("ordinary reason was rewritten")
	}
}
