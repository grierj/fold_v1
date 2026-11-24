package main

import (
	"encoding/json"
	"maps"
	"net/http"
)

// confirmedSend is the body of a schedule press as the sender's confirmation
// sends it (DEC-169): extra, with the message's revision as c reads it now
// and a review c takes now. A read that fails leaves its part out, so a
// refusal decided before them is still the answer.
func confirmedSend(c *apiClient, path string, extra map[string]any) map[string]any {
	out := namedEdit(c, path, extra)
	if w := c.do("GET", path+"/review", nil, false); w.Code == http.StatusOK {
		var r messageReviewResponseJSON
		if json.Unmarshal(w.Body.Bytes(), &r) == nil {
			out["review"] = r.Review.Token
		}
	}
	return out
}

// namedEdit is body with the message's revision as c reads it now, the read an
// editor opened with.
func namedEdit(c *apiClient, path string, body map[string]any) map[string]any {
	out := maps.Clone(body)
	if out == nil {
		out = map[string]any{}
	}
	if w := c.do("GET", path, nil, false); w.Code == http.StatusOK {
		var r messageResponseJSON
		if json.Unmarshal(w.Body.Bytes(), &r) == nil && r.Message.Revision != "" {
			out["revision"] = r.Message.Revision
		}
	}
	return out
}
