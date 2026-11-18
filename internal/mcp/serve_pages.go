package mcp

// The pages ccs serve draws: plain HTML for a phone, no scripts (the CSP
// allows none), every value escaped by html/template. Each write is a form
// posting to /act with the CSRF token, answered first by a confirm page.
const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>ccs · {{.Title}}</title>
<style>
:root{--bg:#fff;--fg:#1a1a1a;--dim:#666;--line:#ddd;--accent:#0a66c2;--warn:#b35900;--bad:#b00020;--card:#f6f6f6}
@media (prefers-color-scheme:dark){:root{--bg:#121212;--fg:#e8e8e8;--dim:#999;--line:#333;--accent:#6cb4ff;--warn:#ffb366;--bad:#ff6b81;--card:#1d1d1d}}
body{background:var(--bg);color:var(--fg);font:15px/1.45 -apple-system,system-ui,sans-serif;margin:0 auto;max-width:900px;padding:0 16px 48px}
nav{display:flex;gap:16px;padding:12px 0;border-bottom:1px solid var(--line);margin-bottom:12px;flex-wrap:wrap}
a{color:var(--accent)}
.row{border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin:8px 0;background:var(--card)}
.row h3{margin:0 0 2px;font-size:16px}
.dim{color:var(--dim);font-size:13px}
.bucket{font-size:12px;text-transform:uppercase;letter-spacing:.04em;color:var(--warn)}
pre{white-space:pre-wrap;word-break:break-word;background:var(--card);border:1px solid var(--line);border-radius:8px;padding:12px;font:13px/1.45 ui-monospace,Menlo,monospace}
form{margin:10px 0}
textarea,input[type=text]{width:100%;box-sizing:border-box;font:inherit;padding:8px;border:1px solid var(--line);border-radius:6px;background:var(--bg);color:var(--fg)}
textarea{min-height:90px}
button{font:inherit;padding:8px 14px;border-radius:6px;border:1px solid var(--accent);background:var(--accent);color:var(--bg);margin:4px 4px 4px 0}
button.quiet{background:transparent;color:var(--accent)}
button.danger{border-color:var(--bad);background:var(--bad)}
.ask{border-left:4px solid var(--warn)}
</style></head><body>
<nav><a href="/">Sessions</a><a href="/prs">PRs</a>{{if .Write}}<a href="/launch">Launch</a>{{end}}<span class="dim">{{if .Write}}read and write{{else}}read-only{{end}}</span></nav>
{{end}}

{{define "foot"}}</body></html>{{end}}

{{define "csrf"}}<input type="hidden" name="csrf" value="{{.}}">{{end}}

{{define "triage"}}{{template "head" .}}
<h2>Sessions</h2>
{{range .Body}}<div class="row">
<div class="bucket">{{.Bucket}}</div>
<h3><a href="/session?id={{.Session}}">{{.Name}}</a></h3>
<div class="dim">{{if .Status}}{{.Status}} {{end}}{{if .Idle}}for {{.Idle}} · {{end}}{{.Reason}}</div>
{{if .LastSaid}}<div>{{.LastSaid}}</div>{{end}}
{{range .OpenPRs}}<div class="dim">{{.Group}} · {{.PR}} · {{.Status}}</div>{{end}}
</div>{{else}}<p class="dim">No sessions.</p>{{end}}
{{template "foot"}}{{end}}

{{define "session"}}{{template "head" .}}{{$csrf := .CSRF}}{{$write := .Write}}{{with .Body}}
<h2>{{if .Name}}{{.Name}}{{else}}{{.ID}}{{end}}</h2>
<div class="dim">{{if .Live}}running{{if .Status}}, {{.Status}}{{end}}{{else}}not running{{end}}</div>
{{if .Ask}}<div class="row ask"><strong>Asking:</strong><pre>{{.Ask.Question}}</pre>
{{if and $write .Ask.Answerable}}{{$ask := .Ask.ID}}{{$id := .ID}}{{$name := .Name}}
{{range $i, $o := .Ask.Options}}<form method="post" action="/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="answer"><input type="hidden" name="id" value="{{$id}}"><input type="hidden" name="name" value="{{$name}}">
<input type="hidden" name="ask" value="{{$ask}}"><input type="hidden" name="option" value="{{inc $i}}">
<button>{{inc $i}}. {{$o}}</button></form>{{end}}
{{else}}<div class="dim">{{if $write}}Answer this one in its tab: only a single single-choice question can be answered from here.{{else}}Read-only: answer it in its tab.{{end}}</div>{{end}}
</div>{{end}}
<p><a href="/session?id={{.ID}}&offset={{.Prev}}">older</a> · <a href="/session?id={{.ID}}">latest</a> · {{if .HasNext}}<a href="/session?id={{.ID}}&offset={{.Next}}">newer</a>{{else}}<span class="dim">newest</span>{{end}}</p>
<pre>{{.Text}}</pre>
{{if and $write .Live .Name}}
<form method="post" action="/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="send"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
<textarea name="text" placeholder="Message {{.Name}}" maxlength="8000"></textarea>
<button>Send</button></form>
<form method="post" action="/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="restart"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
<button class="quiet">Restart</button></form>
<form method="post" action="/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="close"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
<label><input type="checkbox" name="graceful" checked> let it wrap up first</label><br>
<button class="danger">Close</button></form>
{{end}}{{end}}
{{template "foot"}}{{end}}

{{define "text"}}{{template "head" .}}<h2>{{.Body.Heading}}</h2><pre>{{.Body.Text}}</pre>{{template "foot"}}{{end}}

{{define "launch"}}{{template "head" .}}
<h2>Launch a session</h2>
<form method="post" action="/act">{{template "csrf" .CSRF}}
<input type="hidden" name="action" value="launch">
<p><label>Name<br><input type="text" name="name" required></label></p>
<p><label>Folder (absolute path)<br><input type="text" name="cwd" required></label></p>
<p><label>Brief<br><textarea name="brief"></textarea></label></p>
<p><label><input type="checkbox" name="codex"> Codex instead of Claude Code</label></p>
<button>Launch</button></form>
{{template "foot"}}{{end}}

{{define "confirm"}}{{template "head" .}}{{$csrf := .CSRF}}{{with .Body}}
<h2>Confirm</h2><pre>{{.What}}</pre>
<form method="post" action="/act">{{template "csrf" $csrf}}<input type="hidden" name="confirm" value="yes">
<input type="hidden" name="action" value="{{.A.Kind}}"><input type="hidden" name="id" value="{{.A.ID}}"><input type="hidden" name="name" value="{{.A.Name}}">
<input type="hidden" name="text" value="{{.A.Text}}"><input type="hidden" name="ask" value="{{.A.Ask}}"><input type="hidden" name="option" value="{{.A.Option}}">
<input type="hidden" name="cwd" value="{{.A.Cwd}}"><input type="hidden" name="brief" value="{{.A.Brief}}">
{{if .A.Codex}}<input type="hidden" name="codex" value="on">{{end}}{{if .A.Graceful}}<input type="hidden" name="graceful" value="on">{{end}}
<button>Yes, do it</button></form>
<p>{{if .A.ID}}<a href="/session?id={{.A.ID}}">Cancel</a>{{else}}<a href="/">Cancel</a>{{end}}</p>
{{end}}{{template "foot"}}{{end}}

{{define "message"}}{{template "head" .}}{{with .Body}}<h2>{{.Heading}}</h2><pre>{{.Text}}</pre>
<p>{{if .Back}}<a href="/session?id={{.Back}}">Back to the session</a>{{else}}<a href="/">Back to sessions</a>{{end}}</p>{{end}}{{template "foot"}}{{end}}
`
