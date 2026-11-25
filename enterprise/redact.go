package enterprise

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// DemoSecret 是样例截图里的虚构口令。它不是登录口令。
const DemoSecret = "Northstar-Demo-Not-Real"

// DemoLoginPassword 只用于本地演示身份源，不是业务凭据。
const DemoLoginPassword = "demo-login-not-real"

const redactedMark = "［已移除疑似凭据］"

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----.*?-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----`),
	regexp.MustCompile(`(?im)(?P<prefix>^|[\t ])(?:cookie|set-cookie)[\t ]*:[\t ]*[!#$%&'*+.^_` + "`" + `|~0-9a-z-]+=[^\r\n]*(?:\r?\n[\t ]+[^\r\n]*)*`),
	regexp.MustCompile(`(?im)(?P<prefix>^|[\t ])authorization[\t ]*:[\t ]*(?:basic|bearer|digest|negotiate|ntlm|aws4-hmac-sha256|aws|apikey|api-key|token|dpop|oauth|signature|hawk|scram-sha-256)[\t ]+\S[^\r\n]*(?:\r?\n[\t ]+[^\r\n]*)*`),
	regexp.MustCompile(`(?i)\b(?:(?:aws|aliyun|alicloud|tencentcloud|tencent)[_-]?)?(?:access[_-]?key[_-]?(?:id|secret)|secret[_-]?access[_-]?key|secret[_-]?(?:id|key)|client[_-]?secret)["']?[\t ]*[:=][\t ]*["']?[^\s"',;\]}]+["']?`),
	regexp.MustCompile(`\b(?:(?:AKIA|ASIA)[A-Z0-9]{16}|LTAI(?:[A-Za-z0-9]{16}|[A-Za-z0-9]{20})|AKID[A-Za-z0-9]{28,32})\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`),
	regexp.MustCompile(`(?i)(password|passwd|pwd|secret|token|api[_-]?key)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)\bbearer\s+\S+`),
	regexp.MustCompile(regexp.QuoteMeta(DemoSecret)),
}

// Text 是提交后可以给当事人看的文本。含疑似凭据时，展示文本已去掉原文。
type Text struct {
	Display string
	Sealed  string
	Removed bool
}

// Sanitize 拦截自由文本里的疑似凭据。展示文本不再包含原文；封存文本只留给单独的敏感权限。
func Sanitize(raw string) Text {
	raw = strings.TrimSpace(raw)
	if raw == "" || !containsSecret(raw) {
		return Text{Display: raw}
	}
	display := raw
	for _, re := range secretPatterns {
		display = re.ReplaceAllString(display, "${prefix}"+redactedMark)
	}
	display = strings.TrimSpace(display)
	if display == "" {
		display = redactedMark
	}
	return Text{Display: display, Sealed: raw, Removed: true}
}

func containsSecret(raw string) bool {
	for _, re := range secretPatterns {
		if re.MatchString(raw) {
			return true
		}
	}
	return false
}

// Hash 是正文字节的 SHA-256，十六进制小写。多一个空格就是另一个哈希。
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

const CredentialNotice = "内容里有疑似凭据，已从原文移除。申请人、编写者和负责人身份都不能查看这段凭据。"
