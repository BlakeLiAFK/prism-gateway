// 模型列表端点：按调用方可见范围列出网关模型。
package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

func (e *Engine) Models(w http.ResponseWriter, r *http.Request, p string, key Principal) {
	c := e.store.Config()
	ids := []string{}
	for _, m := range c.Models {
		// System One 模型不是对话模型，列在这里只会让客户端拿去发对话请求，
		// 然后收到一个本可以避免的 NO_COMPATIBLE_MODEL。
		if m.Protocol == "systemone" {
			continue
		}
		pr, _ := c.provider(m.ProviderID)
		if m.Enabled && pr.Enabled && key.allows(m.ID) {
			ids = append(ids, m.ID)
		}
	}
	for _, rt := range c.Routes {
		if rt.Enabled && key.allows(rt.ID) {
			ids = append(ids, rt.ID)
		}
	}
	for _, a := range c.Aliases {
		if a.Enabled && key.allows(a.ID) {
			ids = append(ids, a.ID)
		}
	}
	sort.Strings(ids)
	prefix := "/openai/v1/models"
	if p == "messages" {
		prefix = "/anthropic/v1/models"
	}
	specific := strings.TrimPrefix(r.URL.Path, prefix+"/")
	isSpecific := r.URL.Path != prefix
	list := []any{}
	for _, id := range ids {
		if isSpecific && specific != id {
			continue
		}
		if p == "messages" {
			list = append(list, Object{"id": id, "type": "model", "display_name": id, "created_at": "2026-09-19T00:00:00Z"})
		} else {
			list = append(list, Object{"id": id, "object": "model", "created": 0, "owned_by": "prism-gateway"})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if isSpecific {
		if len(list) == 0 {
			protocolError(w, p, 404, "MODEL_NOT_FOUND", "模型不存在或未授权", randomID("req_"))
			return
		}
		json.NewEncoder(w).Encode(list[0])
		return
	}
	if p == "messages" {
		var first, last any
		if len(list) > 0 {
			first = obj(list[0])["id"]
			last = obj(list[len(list)-1])["id"]
		}
		json.NewEncoder(w).Encode(Object{"data": list, "has_more": false, "first_id": first, "last_id": last})
	} else {
		json.NewEncoder(w).Encode(Object{"object": "list", "data": list})
	}
}
