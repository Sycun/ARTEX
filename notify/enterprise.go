package notify

import "strings"

const enterprisePrefix = "enterprise_"

// These messages are safe for channels shared by multiple projects.
var enterpriseNotices = map[string]struct{ title, summary string }{
	"join_requested":     {"加入请求待处理", "有一张新的加入请求。请登录后查看待办。"},
	"join_rejected":      {"加入请求已处理", "有一张加入请求已处理。请登录后查看待办。"},
	"member_added":       {"加入请求已处理", "有一张加入请求已处理。请登录后查看待办。"},
	"original_requested": {"原件申请待处理", "有一张原件申请。请登录后查看待办。"},
	"original_approved":  {"原件申请已处理", "有一张原件申请已处理。请登录后查看。"},
	"original_rejected":  {"原件申请已处理", "有一张原件申请已处理。请登录后查看待办。"},
	"report_submitted":   {"报告待复核", "有一份报告待复核。请登录后查看待办。"},
	"report_returned":    {"报告已退回", "有一份报告已退回。请登录后查看待办。"},
	"report_issued":      {"报告已签发", "有一份报告已签发。请登录后查看待办。"},
}

func EnterpriseSnapshot(event string) (Snapshot, bool) {
	notice, ok := enterpriseNotices[event]
	if !ok {
		return Snapshot{}, false
	}
	return Snapshot{Kind: enterprisePrefix + event, Name: notice.title, Summary: notice.summary}, true
}

func IsEnterpriseEvent(kind string) bool {
	return strings.HasPrefix(kind, enterprisePrefix)
}
