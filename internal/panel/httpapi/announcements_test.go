package httpapi

import (
	"net/http"
	"testing"
)

// ============================================================================
// 公告与帮助文档
// ============================================================================

// TestAnnouncementVisibility 覆盖最关键的一条：**草稿不能被普通用户看到**。
//
// 管理员存草稿是常态（先写好、到点再发），如果列表接口漏了 published 过滤，
// 草稿会直接推给全站用户 —— 这类"说出去就收不回"的事必须由测试守住。
func TestAnnouncementVisibility(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "an1", "password": "an123456", "role": "user"})
	userTok := loginAs(t, ts, "an1", "an123456")

	// ① 发布一条
	code, body := doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "维护通知", "body": "今晚 23:00 停机维护"})
	if code != http.StatusCreated {
		t.Fatalf("创建公告应 201，实际 %d body=%v", code, body)
	}
	if body["id"] == nil {
		t.Errorf("创建公告应返回 id，实际 %v", body)
	}

	// ② 草稿一条（published=false）
	code, _ = doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "草稿", "body": "还没想好", "published": false})
	if code != http.StatusCreated {
		t.Fatalf("存草稿应 201，实际 %d", code)
	}

	// ③ 普通用户只看到已发布的
	code, list := doJSONArr(t, ts, "GET", "/api/announcements", userTok, nil)
	if code != http.StatusOK {
		t.Fatalf("普通用户读公告应 200，实际 %d", code)
	}
	if len(list) != 1 {
		t.Fatalf("普通用户应只看到 1 条已发布公告，实际 %d 条: %v", len(list), list)
	}
	if list[0]["title"] != "维护通知" {
		t.Errorf("看到的是 %v，应为「维护通知」", list[0]["title"])
	}

	// ④ 管理员看得到草稿（否则存了就找不回来）
	_, adminList := doJSONArr(t, ts, "GET", "/api/announcements", adminTok, nil)
	if len(adminList) != 2 {
		t.Errorf("管理员应看到 2 条（含草稿），实际 %d", len(adminList))
	}
}

// TestAnnouncementOrderingPinnedFirst 置顶必须排在前面 —— 这是"置顶"的唯一意义。
func TestAnnouncementOrderingPinnedFirst(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	for _, ti := range []string{"较早的", "较新的"} {
		doJSON(t, ts, "POST", "/api/announcements", adminTok,
			map[string]interface{}{"title": ti, "body": ""})
	}
	// 把**较早**那条置顶
	doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "要置顶的", "body": ""})
	_, list := doJSONArr(t, ts, "GET", "/api/announcements", adminTok, nil)
	if len(list) != 3 {
		t.Fatalf("应有 3 条，实际 %d", len(list))
	}
	// 第三条是最新的（时间倒序第一位），给它置顶
	lastID := list[0]["id"]
	code, _ := doJSON(t, ts, "PUT", "/api/announcements/"+itoa(int64(lastID.(float64))), adminTok,
		map[string]interface{}{"title": "要置顶的", "body": "", "pinned": true})
	if code != http.StatusOK {
		t.Fatalf("置顶应 200，实际 %d", code)
	}
	_, list2 := doJSONArr(t, ts, "GET", "/api/announcements", adminTok, nil)
	if list2[0]["title"] != "要置顶的" {
		t.Errorf("置顶的公告应排第一，实际第一是 %v", list2[0]["title"])
	}
	if list2[0]["pinned"] != true {
		t.Errorf("pinned 字段应为 true，实际 %v", list2[0]["pinned"])
	}
}

// TestAnnouncementWriteRequiresAdmin 写操作只给总管理员。
func TestAnnouncementWriteRequiresAdmin(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "an2", "password": "an123456", "role": "user"})
	userTok := loginAs(t, ts, "an2", "an123456")

	_, created := doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "公告", "body": "内容"})

	if code, _ := doJSON(t, ts, "POST", "/api/announcements", userTok,
		map[string]interface{}{"title": "越权", "body": ""}); code != http.StatusForbidden {
		t.Errorf("普通用户建公告应 403，实际 %d", code)
	}
	id := itoa(int64(created["id"].(float64)))
	if code, _ := doJSON(t, ts, "PUT", "/api/announcements/"+id, userTok,
		map[string]interface{}{"title": "越权改", "body": ""}); code != http.StatusForbidden {
		t.Errorf("普通用户改公告应 403，实际 %d", code)
	}
	if code, _ := doJSON(t, ts, "DELETE", "/api/announcements/"+id, userTok, nil); code != http.StatusForbidden {
		t.Errorf("普通用户删公告应 403，实际 %d", code)
	}
}

// TestAnnouncementValidation 空标题/不存在的 id 要有明确的状态码，不能 500。
func TestAnnouncementValidation(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	if code, _ := doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "   ", "body": "x"}); code != http.StatusBadRequest {
		t.Errorf("空标题应 400，实际 %d", code)
	}
	if code, _ := doJSON(t, ts, "PUT", "/api/announcements/99999", adminTok,
		map[string]interface{}{"title": "x", "body": ""}); code != http.StatusNotFound {
		t.Errorf("改不存在的公告应 404，实际 %d", code)
	}
	if code, _ := doJSON(t, ts, "DELETE", "/api/announcements/99999", adminTok, nil); code != http.StatusNotFound {
		t.Errorf("删不存在的公告应 404，实际 %d", code)
	}
}

// TestAnnouncementDelete 删掉之后列表里就没有了。
func TestAnnouncementDelete(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	_, created := doJSON(t, ts, "POST", "/api/announcements", adminTok,
		map[string]interface{}{"title": "待删", "body": ""})
	id := itoa(int64(created["id"].(float64)))
	if code, _ := doJSON(t, ts, "DELETE", "/api/announcements/"+id, adminTok, nil); code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d", code)
	}
	_, list := doJSONArr(t, ts, "GET", "/api/announcements", adminTok, nil)
	if len(list) != 0 {
		t.Errorf("删除后应剩 0 条，实际 %d", len(list))
	}
}

// ---------------------------------------------------------------------------
// 帮助文档：单行表，所有人可读、只有管理员可写
// ---------------------------------------------------------------------------

func TestHelpDoc(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "hl1", "password": "hl123456", "role": "user"})
	userTok := loginAs(t, ts, "hl1", "hl123456")

	// ① 迁移里插了空文档，未写过时也应当 200 + 空内容（而不是 404/500）
	code, body := doJSON(t, ts, "GET", "/api/help", userTok, nil)
	if code != http.StatusOK {
		t.Fatalf("读帮助应 200，实际 %d body=%v", code, body)
	}
	if body["content"] != "" {
		t.Errorf("初始内容应为空，实际 %q", body["content"])
	}

	// ② 普通用户不能写
	if code, _ := doJSON(t, ts, "PUT", "/api/help", userTok,
		map[string]interface{}{"content": "越权"}); code != http.StatusForbidden {
		t.Errorf("普通用户改帮助应 403，实际 %d", code)
	}

	// ③ 管理员写，所有人立即可见
	const doc = "# 帮助\n- 怎么开机\n- 怎么开端口"
	if code, _ := doJSON(t, ts, "PUT", "/api/help", adminTok,
		map[string]interface{}{"content": doc}); code != http.StatusOK {
		t.Fatalf("管理员保存帮助应 200，实际 %d", code)
	}
	_, body = doJSON(t, ts, "GET", "/api/help", userTok, nil)
	if body["content"] != doc {
		t.Errorf("普通用户应读到刚保存的文档，实际 %q", body["content"])
	}
	if body["updated_by_name"] != "admin" {
		t.Errorf("应记录修改人 admin，实际 %v", body["updated_by_name"])
	}

	// ④ 再写一次是覆盖（单行表，不会变成两条）
	doJSON(t, ts, "PUT", "/api/help", adminTok, map[string]interface{}{"content": "改过了"})
	_, body = doJSON(t, ts, "GET", "/api/help", userTok, nil)
	if body["content"] != "改过了" {
		t.Errorf("应被覆盖为「改过了」，实际 %q", body["content"])
	}
}
