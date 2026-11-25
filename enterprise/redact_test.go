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

func TestSanitizeCredentialFormats(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
	}{
		{"PKCS8 PEM", "-----BEGIN " + "PRIVATE KEY-----\nZmFrZS1rZXk=\n-----END " + "PRIVATE KEY-----"},
		{"OpenSSH PEM with CRLF", "-----BEGIN OPENSSH " + "PRIVATE KEY-----\r\nZmFrZS1rZXk=\r\n-----END OPENSSH " + "PRIVATE KEY-----"},
		{"encrypted PEM", "-----BEGIN ENCRYPTED " + "PRIVATE KEY-----\nZmFrZS1rZXk=\n-----END ENCRYPTED " + "PRIVATE KEY-----"},
		{"JWT", "eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiJkZW1vIn0." + "signature-_"},
		{"AWS access key", "AK" + "IA1234567890ABCDEF"},
		{"AWS temporary access key", "AS" + "IA1234567890ABCDEF"},
		{"AliCloud access key", "LT" + "AI0123456789abcdefghij"},
		{"Tencent access key", "AK" + "ID0123456789abcdefghijklmnopqrstuv"},
		{"AWS secret access key", `AWS_SECRET_ACCESS_KEY="example/secret+key="`},
		{"AliCloud secret", "AccessKeySecret: example-access-secret"},
		{"Tencent secret", "SecretKey = example-tencent-secret"},
		{"Cookie with multiple pairs", "Cookie: sid=one; theme=two"},
		{"Set-Cookie", "sEt-CoOkIe: session=three; Path=/; Secure; HttpOnly"},
		{"Authorization Basic", "Authorization: Basic ZGVtbzpmYWtl"},
		{"Authorization Bearer", "authorization: bearer example-bearer"},
		{"Authorization Digest with continuation", "Authorization: Digest username=\"demo\",\r\n response=\"fake-response\""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := "核验记录 " + tt.value + "\n下一步：复核结果"
			got := Sanitize(in)
			if !got.Removed || got.Sealed != in {
				t.Fatalf("redaction failed: %+v", got)
			}
			want := "核验记录 " + redactedMark + "\n下一步：复核结果"
			if got.Display != want {
				t.Fatalf("display = %q, want %q", got.Display, want)
			}
		})
	}
}

func TestSanitizePreservesBusinessText(t *testing.T) {
	for _, in := range []string{
		"原件申请在审批中，未发现凭据。",
		"Cookie: disabled\nSet-Cookie: absent\nAuthorization: required",
		"X-Cookie: consent=enabled\nCookie policy is documented.",
		"库存编号 AKIA-2048、LTAI02、AKID-BATCH-19。",
		"构建标识 eyJabc.def 只含两段。",
		"-----BEGIN PUBLIC KEY-----\nZmFrZS1rZXk=\n-----END PUBLIC KEY-----",
		"-----BEGIN CERTIFICATE-----\nZmFrZS1jZXJ0\n-----END CERTIFICATE-----",
	} {
		if got := Sanitize(in); got != (Text{Display: in}) {
			t.Errorf("business text changed: %+v", got)
		}
	}
}
