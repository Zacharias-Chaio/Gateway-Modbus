package api

import (
	"errors"
	"net/http"

	"gateway/internal/engine/converter"
)

// 本文件集中定义「保存类接口的结构化校验错误」契约，
// 让设备模型与链路两侧共用同一套前端渲染逻辑。

// FieldIssue 是一条可定位的配置问题。
//
//   - Index   前端表格的行号（设备挂载行 / 属性行）；-1 表示不针对某一行，
//     而是整个表单的字段级错误（如串口名为空、IP 端口非法）。
//   - Field   出问题的表单字段名（前端据此高亮具体输入框）。
//   - Message 面向用户的中文说明。
//
// 后端一次返回全部问题并逐条定位，前端才能把错误标到具体行，
// 而不是弹一句「保存失败」让用户自己去数行号或翻后台日志。
type FieldIssue struct {
	Index   int    `json:"index"`
	Field   string `json:"field,omitempty"`
	Name    string `json:"name,omitempty"`  // 设备名 / 属性名，仅用于展示
	RefID   string `json:"refId,omitempty"` // 设备序号 / 属性 ID，唯一标识
	Message string `json:"message"`
}

// FieldIssues 是 FieldIssue 的集合，实现 error 以便直接返回。
type FieldIssues []FieldIssue

func (f FieldIssues) Error() string {
	if len(f) == 0 {
		return ""
	}
	out := ""
	for i, issue := range f {
		if i > 0 {
			out += "；"
		}
		out += issue.Message
	}
	return out
}

// toFieldIssues 把属性级问题转换为统一的前端可定位结构。
func toFieldIssues(propIssues converter.PropIssues) FieldIssues {
	out := make(FieldIssues, 0, len(propIssues))
	for _, p := range propIssues {
		out = append(out, FieldIssue{
			Index:   p.Index,
			Field:   p.Field,
			Name:    p.PropName,
			RefID:   p.PropID,
			Message: p.Message,
		})
	}
	return out
}

// failFieldIssues 统一返回 422 + issues，供前端逐行/逐字段标注。
func failFieldIssues(w http.ResponseWriter, summary string, issues FieldIssues) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"code":   http.StatusUnprocessableEntity,
		"msg":    summary,
		"issues": issues,
	})
}

// failValidation 把任意校验错误转成 422 + issues。
// 同时支持属性级（converter.PropIssues）与链路/设备级（FieldIssues）。
func failValidation(w http.ResponseWriter, err error) {
	payload := map[string]any{"code": http.StatusUnprocessableEntity, "msg": err.Error()}

	var propIssues converter.PropIssues
	if errors.As(err, &propIssues) {
		payload["issues"] = toFieldIssues(propIssues)
	}
	var fieldIssues FieldIssues
	if errors.As(err, &fieldIssues) {
		payload["issues"] = fieldIssues
	}
	writeJSON(w, http.StatusUnprocessableEntity, payload)
}
