package phone

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

// Every file is served under Prefix with its type, the page's CSP and no
// sniffing; anything else is 404.
func TestHandlerServesTheFiles(t *testing.T) {
	want := map[string]string{
		Prefix:                                "text/html",
		Prefix + "app.js":                     "text/javascript",
		Prefix + "sw.js":                      "text/javascript",
		Prefix + "style.css":                  "text/css",
		Prefix + "manifest.webmanifest":       "application/manifest+json",
		Prefix + "icons/icon-192.png":         "image/png",
		Prefix + "icons/icon-512.png":         "image/png",
		Prefix + "icons/apple-touch-icon.png": "image/png",
	}
	for p, ctype := range want {
		w := get(t, p)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), ctype) {
			t.Errorf("GET %s: %d %q, want 200 %s", p, w.Code, w.Header().Get("Content-Type"), ctype)
		}
		if w.Header().Get("Content-Security-Policy") != CSP || w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("GET %s headers: %v", p, w.Header())
		}
	}
	for _, p := range []string{Prefix + "missing.js", Prefix + "icons", Prefix + "icons/", Prefix + "phone.go", "/elsewhere/app.js"} {
		if w := get(t, p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: %d, want 404", p, w.Code)
		}
	}
}

// The CSP allows only the page's own script, styles, fetches and worker,
// and no framing.
func TestCSP(t *testing.T) {
	for _, d := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(CSP, d) {
			t.Errorf("CSP lacks %q", d)
		}
	}
	for _, bad := range []string{"unsafe-inline", "unsafe-eval", "*", "data:", "http:", "https:"} {
		if strings.Contains(CSP, bad) {
			t.Errorf("CSP allows %q", bad)
		}
	}
}

func read(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(files, name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Agent-supplied text (names, good-at lines, request text, attachment
// names) reaches the page only as text: no script builds markup from a
// string, evaluates code, or loads anything from another origin.
func TestScriptsInsertTextOnly(t *testing.T) {
	sinks := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|setAttribute\(\s*["']on|\.src\s*=|srcdoc|createContextualFragment|DOMParser`)
	for _, name := range []string{"app.js", "sw.js"} {
		src := read(t, name)
		if m := sinks.FindString(src); m != "" {
			t.Errorf("%s uses %q", name, m)
		}
		if strings.Contains(src, "http://") || strings.Contains(src, "https://") {
			t.Errorf("%s names an absolute URL", name)
		}
	}
	if !strings.Contains(read(t, "app.js"), `el("p", "body", item.then.body)`) {
		t.Error("the held card should show a follow-on's text, as text")
	}
	if !strings.Contains(read(t, "app.js"), "textContent") {
		t.Error("app.js should set text with textContent")
	}
}

// The page has no inline script, style or event handler (the CSP would
// block them) and loads only its own files.
func TestPageLoadsOnlyItsOwnFiles(t *testing.T) {
	html := read(t, "index.html")
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, `src="app.js"`) {
			t.Errorf("inline or foreign script: %s", m)
		}
	}
	for _, bad := range []string{"<style", " style=", " onclick=", " onload=", "http://", "https://", "javascript:"} {
		if strings.Contains(html, bad) {
			t.Errorf("index.html contains %q", bad)
		}
	}
	for _, ref := range regexp.MustCompile(`(?:href|src)="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		if _, err := fs.Stat(files, ref[1]); err != nil {
			t.Errorf("index.html references %s, which is not served", ref[1])
		}
	}
	if !strings.Contains(html, `aria-live="polite"`) {
		t.Error("index.html has no live region for announcements")
	}
	if !strings.Contains(html, "viewport-fit=cover") {
		t.Error("index.html does not cover the safe areas")
	}
}

// The manifest makes the page installable from Prefix, with icons that
// are served.
func TestManifest(t *testing.T) {
	var m struct {
		StartURL string `json:"start_url"`
		Scope    string `json:"scope"`
		Display  string `json:"display"`
		Icons    []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal([]byte(read(t, "manifest.webmanifest")), &m); err != nil {
		t.Fatal(err)
	}
	if m.StartURL != "./" || m.Scope != "./" || m.Display != "standalone" || len(m.Icons) == 0 {
		t.Fatalf("manifest = %+v", m)
	}
	for _, ic := range m.Icons {
		if _, err := fs.Stat(files, ic.Src); err != nil {
			t.Errorf("manifest icon %s is not served", ic.Src)
		}
	}
}

// Deny comes before Approve on every held card, and both have accessible
// names.
func TestDenyBeforeApprove(t *testing.T) {
	src := read(t, "app.js")
	deny, approve := strings.Index(src, `el("button", "deny"`), strings.Index(src, `el("button", "approve"`)
	if deny < 0 || approve < 0 || deny > approve {
		t.Fatalf("deny at %d, approve at %d: Deny must be built first", deny, approve)
	}
	if !strings.Contains(src, `actions.append(deny, approve)`) {
		t.Error("Deny must come first in the card's actions")
	}
	if strings.Count(src, `setAttribute("aria-label"`) < 2 {
		t.Error("Deny and Approve need accessible names")
	}
	if !strings.Contains(read(t, "style.css"), "min-height: 44px") {
		t.Error("buttons must be at least 44px tall")
	}
}

// The page joins the phone as kind phone through the relay's own invite
// and join routes, never under the name of another agent (an invite for an
// existing name would move that agent to the phone), and asks and follows
// answers through the relay's routes.
func TestJoinAndAskInThePage(t *testing.T) {
	app := read(t, "app.js")
	for _, want := range []string{`"/v1/admin/invite"`, `"/v1/join"`, `"/v1/send"`, `"/v1/trace?`, `"/answer"`} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	if !regexp.MustCompile(`"/v1/admin/invite"[^;]*kind:\s*"phone"`).MatchString(app) {
		t.Error("the page must invite this phone as kind phone")
	}
	if !strings.Contains(app, `kind !== "phone"`) {
		t.Error("the page must refuse a name another kind of agent holds")
	}
	html := read(t, "index.html")
	for _, want := range []string{"Join this phone", `role="tablist"`, `id="ask-body"`, `for="ask-body"`, `for="join-name"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	// Held asks stay above the tabs that later views are added to.
	held, tabs := strings.Index(html, `id="held"`), strings.Index(html, `role="tablist"`)
	if held < 0 || tabs < 0 || held > tabs {
		t.Errorf("held at %d, tabs at %d: held asks must come before the tabs", held, tabs)
	}
}

// Every push shows a notification with fixed text that opens the held
// list, and the page asks for notification permission only from a tap.
func TestPushAlertsInThePage(t *testing.T) {
	sw := read(t, "sw.js")
	for _, want := range []string{`addEventListener("push"`, "showNotification", `addEventListener("notificationclick"`, "#held", "A request is waiting for your approval"} {
		if !strings.Contains(sw, want) {
			t.Errorf("sw.js lacks %q", want)
		}
	}
	if strings.Contains(sw, "msg.body") || strings.Contains(sw, "msg.title") {
		t.Error("sw.js shows text from the push payload; it must show only its fixed text")
	}
	app := read(t, "app.js")
	if strings.Count(app, "requestPermission") != 1 {
		t.Fatal("app.js should ask for permission in exactly one place")
	}
	_, turnOn, ok := strings.Cut(app, "async function turnOnAlerts")
	if !ok {
		t.Fatal("app.js has no turnOnAlerts")
	}
	body, _, ok := strings.Cut(turnOn, "\n}\n")
	if !ok || !strings.Contains(body, "requestPermission") {
		t.Error("permission is asked outside the Turn on alerts tap")
	}
	if !strings.Contains(app, `"PUT", "/v1/admin/push/subscription"`) {
		t.Error("the page must send its subscription to the relay")
	}
}

// A room card names the request to answer for a question: on the
// question post (its reply_to) and on the asking post's mention line.
func TestRoomQuestionNamesRequest(t *testing.T) {
	app := read(t, "app.js")
	if !strings.Contains(app, `"question, answer with request " + id`) {
		t.Error("app.js must label a question with its request id")
	}
	if body := funcBody(t, app, "postTag"); !strings.Contains(body, "questionTag(p.reply_to)") {
		t.Errorf("postTag must name the question's request:\n%s", body)
	}
	if body := funcBody(t, app, "postCard"); !strings.Contains(body, "needs_input") || !strings.Contains(body, "questionTag(m.request_id)") {
		t.Errorf("postCard must name a questioned mention's request:\n%s", body)
	}
}

// A held card rebuilds when the relay's text for it changes: approval needs
// the whole text showing, so a card must not stay on a cut preview once the
// relay hands it a fresh nonce for the full body.
func TestHeldCardFollowsItsText(t *testing.T) {
	app := read(t, "app.js")
	if body := funcBody(t, app, "heldText"); !strings.Contains(body, "item.body") || !strings.Contains(body, "item.then.body") {
		t.Error("heldText must cover the body and the follow-on text")
	}
	if body := funcBody(t, app, "mergeHeld"); !strings.Contains(body, "heldText(old) !== heldText(item)") || !strings.Contains(body, "replaceWith(heldCard(entry))") {
		t.Error("mergeHeld must rebuild a card whose text changed")
	}
}

// funcBody is the source of the top-level function name in src, up to its
// closing brace at the start of a line.
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(src, "function "+name+"(")
	if !ok {
		t.Fatalf("app.js has no function %s", name)
	}
	body, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatalf("function %s does not end", name)
	}
	return body
}

// Send stays off while an ask is on its way, so a poll that renders in
// the meantime cannot turn it back on for a second tap.
func TestAskSendOffWhileSending(t *testing.T) {
	app := read(t, "app.js")
	if !strings.Contains(funcBody(t, app, "renderAsk"), `$("ask-send").disabled = sending ||`) {
		t.Error("renderAsk must keep Send off while an ask is being sent")
	}
	send := funcBody(t, app, "sendAsk")
	if !strings.Contains(send, "sending = true") || !strings.Contains(send, "sending = false") {
		t.Error("sendAsk must mark the send in flight and clear it when done")
	}
}

// A stalled request ends after a timeout and counts as offline, so one
// hung fetch cannot keep the poll from running again.
func TestAPITimesOut(t *testing.T) {
	body := funcBody(t, read(t, "app.js"), "api")
	for _, want := range []string{"AbortController", "abort()", "signal"} {
		if !strings.Contains(body, want) {
			t.Errorf("api lacks %s", want)
		}
	}
}

// Turn off is remembered on this device: opening the page again shows Turn
// on alerts rather than making a new subscription, until the owner taps it.
func TestAlertsOffIsRemembered(t *testing.T) {
	app := read(t, "app.js")
	if !strings.Contains(funcBody(t, app, "turnOffAlerts"), "alertsOff(true)") {
		t.Error("Turn off must remember that alerts are off on this device")
	}
	if !strings.Contains(funcBody(t, app, "turnOnAlerts"), "alertsOff(false)") {
		t.Error("Turn on alerts must clear the remembered off")
	}
	push := funcBody(t, app, "pushState")
	off, sub := strings.Index(push, "alertsOff()"), strings.Index(push, "subscribe(reg")
	if off < 0 || sub < 0 || off > sub {
		t.Error("pushState must check the remembered off before it subscribes")
	}
}

// The page refuses a phone agent's name unless it is this device's own
// phone agent, since an invite for it would move it to this device.
func TestJoinRefusesAnotherPhone(t *testing.T) {
	body := funcBody(t, read(t, "app.js"), "join")
	if !strings.Contains(body, "remembered()") || !strings.Contains(body, "another phone") {
		t.Error("join must refuse a phone agent this device does not already have")
	}
}

// The page's invite asks the relay to refuse a name another device's agent
// holds (new_only), so a phone that joined since the last roster read is
// not moved here, and a refusal tells the owner to pick another name.
func TestJoinChecksTheNameOnTheRelay(t *testing.T) {
	body := funcBody(t, read(t, "app.js"), "join")
	if !regexp.MustCompile(`"/v1/admin/invite"[^;]*new_only:`).MatchString(body) {
		t.Error("join must mint its invite with new_only")
	}
	if !strings.Contains(body, "409") || !strings.Contains(body, "Pick a different name") {
		t.Error("join must explain a 409 from the relay")
	}
}

// A refresh updates the sent cards in place and moves only cards that are
// out of place, so a focused answer box is never detached and keeps the
// phone's keyboard open.
func TestSentListKeepsCardsInPlace(t *testing.T) {
	body := funcBody(t, read(t, "app.js"), "renderSent")
	if strings.Count(body, "replaceChildren(") > strings.Count(body, "answerBox.replaceChildren(") {
		t.Error("renderSent must not rebuild the list")
	}
	if !strings.Contains(body, "!== entry.li") || !strings.Contains(body, "insertBefore(") {
		t.Error("renderSent must insert a card only where it is out of place")
	}
	if !strings.Contains(body, ".remove()") {
		t.Error("renderSent must remove cards that leave the list")
	}
}

// The sent list asks the relay for this phone's own chains, filtered
// before the limit, so newer team traffic cannot hide an open ask.
func TestSentListAsksForItsOwnChains(t *testing.T) {
	body := funcBody(t, read(t, "app.js"), "refreshSent")
	if !strings.Contains(body, `&from=" + encodeURIComponent(state.me)`) {
		t.Error("refreshSent must ask /v1/trace for this phone's own chains with from")
	}
}

// Opening the page checks the relay's current push key even when a
// subscription exists, so one made with a replaced key is renewed.
func TestPushStateChecksTheKey(t *testing.T) {
	app := read(t, "app.js")
	if strings.Contains(funcBody(t, app, "pushState"), "getSubscription()) ||") {
		t.Error("pushState must not reuse a subscription without checking the relay's key")
	}
	if !strings.Contains(funcBody(t, app, "subscribe"), "applicationServerKey") {
		t.Error("subscribe must subscribe with the relay's current key")
	}
}

// Back in the foreground, the held cards stay off until a fresh held
// listing lands, so a stale card cannot be decided.
func TestHeldStaleUntilRefreshed(t *testing.T) {
	app := read(t, "app.js")
	if !strings.Contains(funcBody(t, app, "buttonsEnabled"), "state.heldFresh") {
		t.Error("buttonsEnabled must require a fresh held listing")
	}
	if !strings.Contains(funcBody(t, app, "schedule"), "state.heldFresh = false") {
		t.Error("schedule must mark the held list stale when the page hides or returns")
	}
	if !strings.Contains(funcBody(t, app, "refresh"), "state.heldFresh = ") {
		t.Error("refresh must mark the held list fresh after a good listing")
	}
}

// Until whoami has answered, the page cannot tell whether an ask came from
// this phone, so approving asks for confirmation.
func TestApproveConfirmsWhileIdentityUnknown(t *testing.T) {
	if !strings.Contains(funcBody(t, read(t, "app.js"), "decide"), "!state.meChecked") {
		t.Error("decide must confirm an approval while this phone's agent is unknown")
	}
}

// The rooms tab manages members through the admin routes, reads after a
// cursor and posts as the phone's agent, draws post bodies as text, and asks
// before adding a member that sends the thread to an outside vendor.
func TestRoomsInThePage(t *testing.T) {
	app := read(t, "app.js")
	for _, want := range []string{
		`"/v1/admin/rooms"`,
		`"/v1/admin/rooms/"`,
		`"/members"`,
		`"/v1/rooms/"`,
		`"/posts?after="`,
		`"/posts"`,
		`" agent: the posts it reads or is sent go to that vendor, under your account there. Add it to "`,
		`el("p", "body", p.body)`,
	} {
		if !strings.Contains(app, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	html := read(t, "index.html")
	for _, want := range []string{`id="tab-rooms"`, `aria-controls="rooms"`, `for="room-body"`, `for="room-new"`, `for="room-add-agent"`, `for="room-pick"`} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
}

// The rooms tab shows an error with no room open (the first room's create
// fails before any room-view exists), lets a post answered after a room
// switch touch only its own draft and room, and rechecks every open
// mention in turn rather than only the newest.
func TestRoomsTabEdgeCases(t *testing.T) {
	html := read(t, "index.html")
	errAt, viewAt := strings.Index(html, `id="room-error"`), strings.Index(html, `id="room-view"`)
	if errAt < 0 || viewAt < 0 || errAt > viewAt {
		t.Error("room-error must sit outside room-view, which is hidden until a room is open")
	}
	app := read(t, "app.js")
	post := funcBody(t, app, "postToRoom")
	for _, want := range []string{"stillOpen()", `$("room-body").value === draft`} {
		if !strings.Contains(post, want) {
			t.Errorf("postToRoom lacks %s", want)
		}
	}
	if !strings.Contains(funcBody(t, app, "recheckMentions"), "state.roomRecheckAt") {
		t.Error("recheckMentions must rotate through the open mentions")
	}
}

// A room's unsent draft stays with that room: every switch goes through
// switchRoom, which keeps the box's text under the room being left and
// shows the next room's own draft (or nothing), so a draft never posts to
// a room it was not written for.
func TestRoomDraftPerRoom(t *testing.T) {
	app := read(t, "app.js")
	sw := funcBody(t, app, "switchRoom")
	for _, want := range []string{"state.roomDrafts.set(state.room", `$("room-body")`, `state.roomDrafts.get(name) || ""`, "resetRoom()"} {
		if !strings.Contains(sw, want) {
			t.Errorf("switchRoom lacks %s", want)
		}
	}
	if n := strings.Count(app, "state.room = "); n != 1 {
		t.Errorf("state.room is set in %d places; every switch must go through switchRoom", n)
	}
	if !strings.Contains(funcBody(t, app, "postToRoom"), "state.roomDrafts.delete(room)") {
		t.Error("postToRoom must drop a sent draft kept for a room switched away from")
	}
}

// tasksScript is the part of app.js that draws and runs the Tasks tab.
func tasksScript(t *testing.T) string {
	t.Helper()
	_, rest, ok := strings.Cut(read(t, "app.js"), "// ---- Tasks:")
	if !ok {
		t.Fatal("app.js has no Tasks section")
	}
	body, _, ok := strings.Cut(rest, "\nfunction render() {")
	if !ok {
		t.Fatal("the Tasks section does not end at render")
	}
	return body
}

// The served page has the Tasks tab in the tablist with its role and the
// panel it controls, the Done and Dropped groups are buttons with an
// expanded state, and the page's CSP is exactly what it was.
func TestTasksTabInThePage(t *testing.T) {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; manifest-src 'self'; worker-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if CSP != csp {
		t.Errorf("the page's CSP changed:\n got %s\nwant %s", CSP, csp)
	}
	w := get(t, Prefix)
	if got := w.Header().Get("Content-Security-Policy"); got != csp {
		t.Errorf("served CSP = %q", got)
	}
	html := w.Body.String()
	_, list, ok := strings.Cut(html, `role="tablist"`)
	if !ok {
		t.Fatal("no tablist")
	}
	list, _, _ = strings.Cut(list, "</div>")
	if !regexp.MustCompile(`<button id="tab-tasks" class="tab" type="button" role="tab" aria-selected="false" aria-controls="tasks">`).MatchString(list) {
		t.Errorf("the tablist lacks the Tasks tab with its role and controls:\n%s", list)
	}
	if n := strings.Count(list, `role="tab"`); n != 4 {
		t.Errorf("%d tabs in the tablist, want Team, Ask, Rooms and Tasks", n)
	}
	for _, want := range []string{
		`<section id="tasks" role="tabpanel" aria-labelledby="tab-tasks" hidden>`,
		`id="tasks-count"`,
		`for="task-title"`, `for="task-detail"`, `for="task-for"`,
		`id="task-add-error" class="error" role="alert"`,
		`id="tasks-empty"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %s", want)
		}
	}
	for _, name := range []string{"proposed", "answer", "returned", "working", "open", "yours", "done", "dropped"} {
		if !strings.Contains(html, `<ol id="tg-`+name+`" class="cards"`) || !strings.Contains(html, `id="tg-`+name+`-count" class="count"`) {
			t.Errorf("index.html lacks the %s group with its count", name)
		}
	}
	// The groups come in the order a task is sorted into them.
	at := -1
	for _, title := range []string{">Proposed <", ">Needs your answer <", ">Returned <", ">Working <", ">Open <", ">Yours <", ">Done <", ">Dropped <"} {
		i := strings.Index(html, title)
		if i < at {
			t.Errorf("group %s is out of order", title)
		}
		at = i
	}
	for _, name := range []string{"done", "dropped"} {
		if !strings.Contains(html, `<button id="tg-`+name+`-toggle" class="disclose" type="button" aria-expanded="false" aria-controls="tg-`+name+`">`) {
			t.Errorf("the %s group is not behind a button with an expanded state", name)
		}
		if !strings.Contains(html, `<ol id="tg-`+name+`" class="cards" aria-labelledby="tg-`+name+`-toggle" hidden>`) {
			t.Errorf("the %s list does not start hidden", name)
		}
	}
	app := read(t, "app.js")
	if !strings.Contains(app, `toggle.setAttribute("aria-expanded", open ? "true" : "false")`) {
		t.Error("the Done and Dropped buttons must keep their expanded state")
	}
	if !strings.Contains(funcBody(t, app, "renderTabs"), `"tasks"`) {
		t.Error("renderTabs must show and select the Tasks tab")
	}
	if !strings.Contains(funcBody(t, app, "refresh"), "refreshTasks()") {
		t.Error("the page's poll must refresh the task list")
	}
}

// Task titles, details, results, comments, history and questions reach the
// page only as text: the Tasks code has no HTML sink, and builds every node
// from text.
func TestTaskTextIsInsertedAsText(t *testing.T) {
	tasks := tasksScript(t)
	if m := regexp.MustCompile(`innerHTML|outerHTML|insertAdjacentHTML|document\.write|\beval\(|new Function|DOMParser|createContextualFragment|srcdoc|\.src\s*=|\.href\s*=`).FindString(tasks); m != "" {
		t.Errorf("the Tasks code uses %q", m)
	}
	if strings.Contains(tasks, "createElement") {
		t.Error("the Tasks code must build nodes with el, which sets text as text")
	}
	for _, want := range []string{
		`button("task-head", title,`,
		`setText(c.detail, full ? full.detail`,
		`setText(c.result, result)`,
		`el("p", "body", ev.body)`,
		`el("p", "meta", "Question: " + ask.question)`,
		`setText(row.good, a.good_at ?`,
		`setText(c.error, entry.error)`,
	} {
		if !strings.Contains(tasks, want) {
			t.Errorf("the Tasks code lacks %s", want)
		}
	}
	if body := funcBody(t, read(t, "app.js"), "setText"); !strings.Contains(body, "node.textContent = text") {
		t.Error("setText must set text with textContent")
	}
}

// Approve, done, reopen, drop and reset each go through decideTask, which
// sends the nonce the task listing issued for that task and decision, and
// only while a fresh listing's nonce is live. A refusal shows on the row,
// and an expired nonce reloads the list.
func TestTaskDecisionsSendNonce(t *testing.T) {
	app := read(t, "app.js")
	tasks := tasksScript(t)
	decide := funcBody(t, app, "decideTask")
	for _, want := range []string{
		`const nonce = task[verb + "_nonce"]`,
		`api("POST", taskPath(task.id) + "/" + verb, { nonce })`,
		"if (!taskReady(entry, verb)) return;",
		"res.status === 409",
		"STALE_TASK_BUTTON",
		"entry.error = ",
		"refreshTasks()",
	} {
		if !strings.Contains(decide, want) {
			t.Errorf("decideTask lacks %s", want)
		}
	}
	for _, verb := range []string{"approve", "done", "reopen", "drop", "reset"} {
		if !regexp.MustCompile(`\n  ` + verb + `: \["`).MatchString(tasks) {
			t.Errorf("TASK_DECISIONS lacks %s", verb)
		}
		if n := strings.Count(tasks, `decideTask(entry, "`+verb+`")`); n != 1 {
			t.Errorf("%d buttons call decideTask for %s, want 1", n, verb)
		}
		// No other code path reaches a decision route.
		if strings.Contains(tasks, `"/`+verb+`"`) {
			t.Errorf("the Tasks code names the %s route outside decideTask", verb)
		}
	}
	ready := funcBody(t, app, "taskReady")
	for _, want := range []string{"state.tasksFresh", "!taskOff()", `entry.task[verb + "_nonce"]`, "nonceLive(entry.task)"} {
		if !strings.Contains(ready, want) {
			t.Errorf("taskReady lacks %s", want)
		}
	}
	if !strings.Contains(app, `const STALE_TASK_BUTTON = "this task button has expired"`) {
		t.Error("the page must know the relay's expired-nonce refusal")
	}
	if !strings.Contains(funcBody(t, app, "schedule"), "state.tasksFresh = false") {
		t.Error("schedule must mark the task list stale when the page hides or returns")
	}
	if !strings.Contains(funcBody(t, app, "refreshTasksOnce"), `api("GET", "/v1/admin/tasks"`) {
		t.Error("the nonces must come from the admin task listing")
	}
}

// Task status changes and the owner's own decisions are announced through
// the page's live region, refusals are alerts on the row, and every action
// button's label names its task.
func TestTaskChangesAreAnnounced(t *testing.T) {
	app := read(t, "app.js")
	merge := funcBody(t, app, "mergeTasks")
	if !strings.Contains(merge, "entry.task.status !== task.status") || !strings.Contains(merge, "announce(") {
		t.Error("mergeTasks must announce a task whose status changed")
	}
	for fn, want := range map[string]string{
		"decideTask":  `announce(TASK_DECISIONS[verb][0] + " " + task.title`,
		"assignTask":  `announce("Assigned " + task.title`,
		"commentTask": `announce("Comment added to " + entry.task.title`,
		"addTask":     `announce("Added task " + title`,
	} {
		if !strings.Contains(funcBody(t, app, fn), want) {
			t.Errorf("%s lacks %s", fn, want)
		}
	}
	if !strings.Contains(funcBody(t, app, "announce"), `$("live")`) {
		t.Error("announce must write to the live region")
	}
	card := funcBody(t, app, "taskCard")
	if !strings.Contains(card, `c.error.setAttribute("role", "alert")`) {
		t.Error("a row's refusal text must be an alert")
	}
	for _, b := range []string{"drop", "reopen", "reset", "done", "approve", "commentSend", "pickCancel"} {
		if !regexp.MustCompile(`c\.` + b + `\.setAttribute\("aria-label", "[^"]+" \+ title\)`).MatchString(card) {
			t.Errorf("the %s button's label must name the task", b)
		}
	}
	update := funcBody(t, app, "updateTaskCard")
	if !strings.Contains(update, `c.assign.setAttribute("aria-label", verb + " " + task.title)`) {
		t.Error("the assign button's label must name the task")
	}
	if !strings.Contains(update, `c.head.setAttribute("aria-expanded"`) {
		t.Error("a row's title button must say whether the row is expanded")
	}
}

// The answer box is drawn only for a question on an ask this phone's agent
// sent, which the page learns by reading the ask as that agent. Any other
// question stays with the working tasks.
func TestTaskAnswerBoxOnlyForOwnAsk(t *testing.T) {
	app := read(t, "app.js")
	if !strings.Contains(funcBody(t, app, "updateTaskCard"), `taskAnswerBox(entry, task.status === "question" && ask && ask.mine ? ask : null)`) {
		t.Error("updateTaskCard must draw the answer box only for this phone's own question")
	}
	if n := strings.Count(app, "taskAnswerBox(entry,"); n != 2 {
		t.Errorf("taskAnswerBox is named %d times, want its definition and one call", n)
	}
	asks := funcBody(t, app, "loadTaskAsks")
	for _, want := range []string{`api("GET", "/v1/requests/" + encodeURIComponent(id))`, "res.data.request.from === state.me", `res.data.status !== "needs_input"`, "mine: true"} {
		if !strings.Contains(asks, want) {
			t.Errorf("loadTaskAsks lacks %s", want)
		}
	}
	if n := strings.Count(app, "mine: true"); n != 1 {
		t.Errorf("an ask is marked as this phone's in %d places, want 1", n)
	}
	if !strings.Contains(funcBody(t, app, "taskGroup"), `return ask && ask.mine ? "answer" : "working";`) {
		t.Error("a question this phone cannot answer must stay with the working tasks")
	}
	box := funcBody(t, app, "taskAnswerBox")
	if !strings.Contains(box, `api("POST", "/v1/requests/" + encodeURIComponent(req) + "/answer", { body })`) {
		t.Error("the answer box must use the relay's request answer route")
	}
	if !strings.Contains(box, "if (!ask) {") {
		t.Error("taskAnswerBox must take the box away when the question is not this phone's")
	}
}

// A refresh updates rows in place by task id and moves only a row that is
// out of place and not in use; assigning goes through the admin assign
// route as this phone's agent, with a way to join when there is none; and
// every task action is off while the page is offline.
func TestTaskRowsStayInPlace(t *testing.T) {
	app := read(t, "app.js")
	render := funcBody(t, app, "renderTasks")
	if strings.Contains(render, "replaceChildren(") {
		t.Error("renderTasks must not rebuild a list")
	}
	for _, want := range []string{"if (at === entry.li) return;", "insertBefore(", "!taskPinned(entry)", ".remove()", "a.task.id - b.task.id"} {
		if !strings.Contains(render, want) {
			t.Errorf("renderTasks lacks %s", want)
		}
	}
	pinned := funcBody(t, app, "taskPinned")
	for _, want := range []string{"entry.picker", "comment.value", "answerInput.value", "document.activeElement"} {
		if !strings.Contains(pinned, want) {
			t.Errorf("taskPinned lacks %s", want)
		}
	}
	if !strings.Contains(funcBody(t, app, "mergeTasks"), "entry.task = task") {
		t.Error("mergeTasks must keep a row and give it the listing's task")
	}
	assign := funcBody(t, app, "assignTask")
	if !strings.Contains(assign, `api("POST", taskPath(task.id) + "/assign", { to })`) || !strings.Contains(assign, "!state.me") {
		t.Error("assignTask must use the admin assign route, as this phone's agent")
	}
	if !strings.Contains(assign, `entry.error = "Not assigned: " + relayError(res)`) {
		t.Error("a refused assign must show the relay's reason on the row")
	}
	update := funcBody(t, app, "updateTaskCard")
	for _, want := range []string{"c.assign.disabled = off || entry.busy || !state.me", "c.join.hidden = !unjoined", "c.commentNote.hidden = !task.current_request", "c.commentSend.disabled = off"} {
		if !strings.Contains(update, want) {
			t.Errorf("updateTaskCard lacks %s", want)
		}
	}
	if !strings.Contains(funcBody(t, app, "renderTaskForm"), `$("task-add").disabled = addingTask || taskOff()`) {
		t.Error("Add task must be off while offline or while an add is on its way")
	}
	if !strings.Contains(app, "const taskOff = () => state.offline || state.notAdmin;") {
		t.Error("task actions must be off while offline")
	}
	candidates := funcBody(t, app, "taskCandidates")
	if !strings.Contains(candidates, "!a.guest_until") || !strings.Contains(candidates, "NO_TASK_KINDS.has(a.kind)") {
		t.Error("the assign picker must leave out guests and the kinds that take no tasks")
	}
	if !strings.Contains(app, `const NO_TASK_KINDS = new Set(["phone", "history", "notes", "pass", "council"]);`) {
		t.Error("the assign picker must leave out phone, history, notes, pass and council agents")
	}
}
