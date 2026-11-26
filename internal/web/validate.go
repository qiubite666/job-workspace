package web

import "net/http"

// writeValidationFailed 输出 422 字段级校验错误。
//
// fields 的键是请求体里的字段名，值是给人看的中文说明；前端按字段名回填到
// 对应输入框下方，不依赖顺序。
func writeValidationFailed(w http.ResponseWriter, fields map[string]string) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"ok":     false,
		"error":  "VALIDATION_FAILED",
		"fields": fields,
	})
}
