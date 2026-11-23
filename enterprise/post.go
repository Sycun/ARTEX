package enterprise

// effectivePost 把库存岗位、基础角色和职责收成员工、复核人、领导或只读。
// 显式岗位优先。没有显式岗位时，领导职责和领导角色都算领导；复核职责算复核人。
func effectivePost(role string, duties []string, stored string) string {
	switch stored {
	case "employee", "reviewer", "leader", "reader":
		return stored
	}
	switch {
	case role == "leader" || hasDuty(duties, "lead"):
		return "leader"
	case role == "reader":
		return "reader"
	case hasDuty(duties, "review") && role != "owner":
		return "reviewer"
	case role == "owner" || role == "member":
		return "employee"
	default:
		return ""
	}
}

func postLabel(post string) string {
	switch post {
	case "employee":
		return "员工"
	case "reviewer":
		return "复核人"
	case "leader":
		return "领导"
	case "reader":
		return "只读"
	default:
		return ""
	}
}

func readOnlyRole(role string) bool {
	return role == "reader" || role == "leader"
}

func canEdit(role string, duties []string, stored string) bool {
	return effectivePost(role, duties, stored) == "employee"
}
