package mcp

// The pages ccs serve draws, mobile first: a sticky bar with the "waiting
// on you" count, session cards grouped by what they need, a session as
// message bubbles with its held prompt on top and a composer pinned to the
// bottom. No scripts (the CSP allows none): the sessions page and a
// waiting session's page refresh themselves with a meta refresh. Every
// value is escaped by html/template, every link and form starts with
// $.Base (a preview's secret path), and each write is a form posting to
// /act with the CSRF token, answered first by a confirm page.
const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="robots" content="noindex">
{{if .Refresh}}<meta http-equiv="refresh" content="10">{{end}}
<meta name="color-scheme" content="light dark">
<meta name="theme-color" content="#f6f7f9" media="(prefers-color-scheme: light)">
<meta name="theme-color" content="#0f1115" media="(prefers-color-scheme: dark)">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-title" content="ccs">
<meta name="apple-mobile-web-app-status-bar-style" content="default">
<link rel="manifest" href="{{$.Base}}/manifest.webmanifest" crossorigin="use-credentials">
<link rel="icon" href="{{$.Base}}/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="{{$.Base}}/icon/180">
<title>{{.Title}} · ccs</title>
<style>
:root{--bg:#f6f7f9;--surface:#fff;--fg:#14161a;--dim:#5f6672;--line:#e3e6ea;--accent:#2563eb;--accent-fg:#fff;--ok:#1a7f37;--warn:#b26a00;--bad:#c62828;--ext:#6d4bd1;--done:#8a919c;--you:#2563eb;--you-fg:#fff;--bubble:#fff;--code:#eef1f5;--hold:#fff7e6;--hold-line:#f0c36d}
@media (prefers-color-scheme:dark){:root{--bg:#0f1115;--surface:#171a21;--fg:#e8eaed;--dim:#9aa1ad;--line:#262b34;--accent:#5b9dff;--accent-fg:#0b1220;--ok:#3fb950;--warn:#e3a008;--bad:#f0616d;--ext:#a78bfa;--done:#6b7280;--you:#2b5fd9;--you-fg:#fff;--bubble:#1c2029;--code:#11141a;--hold:#2a2212;--hold-line:#8a6a1f}}
*{box-sizing:border-box}
html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--bg);color:var(--fg);font:16px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,system-ui,sans-serif;overflow-wrap:anywhere}
a{color:var(--accent);text-decoration:none}
.wrap{max-width:720px;margin:0 auto;padding:0 16px 24px}
.bar{position:sticky;top:0;z-index:10;background:var(--bg);border-bottom:1px solid var(--line);padding:env(safe-area-inset-top) 0 0}
.bar .wrap{display:flex;align-items:center;gap:10px;padding-top:10px;padding-bottom:10px;flex-wrap:wrap}
.brand{font-weight:700;font-size:18px;color:var(--fg)}
.badge{display:inline-flex;align-items:center;min-height:32px;padding:4px 12px;border-radius:999px;background:var(--bad);color:#fff;font-size:14px;font-weight:600}
.badge.zero{background:var(--surface);color:var(--dim);border:1px solid var(--line)}
.updated{margin-left:auto;color:var(--dim);font-size:13px}
nav{display:flex;gap:4px;width:100%;overflow-x:auto}
nav a{display:inline-flex;align-items:center;min-height:40px;padding:0 14px;border-radius:10px;color:var(--fg);font-weight:500}
nav a:active{background:var(--surface)}
.mode{display:inline-flex;align-items:center;color:var(--dim);font-size:13px;padding:0 6px}
h1{font-size:22px;line-height:1.25;margin:18px 0 4px}
h2{font-size:13px;font-weight:600;text-transform:uppercase;letter-spacing:.06em;color:var(--dim);margin:22px 0 8px}
.dim{color:var(--dim);font-size:14px}
.card{display:block;background:var(--surface);border:1px solid var(--line);border-radius:14px;padding:14px 16px;margin:10px 0;color:var(--fg)}
a.card:active{border-color:var(--accent)}
.card .top{display:flex;align-items:center;gap:10px}
.card .name{font-weight:600;font-size:17px;flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.card .age{color:var(--dim);font-size:14px;white-space:nowrap}
.card .reason{color:var(--dim);font-size:14px;margin-top:4px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.card .said{margin-top:6px;font-size:15px;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.dot{width:10px;height:10px;border-radius:50%;flex:none;background:var(--done)}
.dot.waiting_on_user{background:var(--bad)}.dot.working{background:var(--warn)}.dot.waiting_on_external{background:var(--ext)}.dot.stalled{background:var(--warn)}.dot.idle{background:var(--ok)}
.chips{display:flex;flex-wrap:wrap;gap:6px;margin-top:8px}
.chip{display:inline-block;font-size:13px;padding:3px 10px;border-radius:999px;border:1px solid var(--line);color:var(--dim);max-width:100%;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.chip.ready{border-color:var(--ok);color:var(--ok)}.chip.waiting{border-color:var(--warn);color:var(--warn)}.chip.needs_you{border-color:var(--bad);color:var(--bad)}
.hold{background:var(--hold);border-color:var(--hold-line)}
.hold .title{font-weight:700;display:flex;justify-content:space-between;gap:8px}
.btn{display:flex;align-items:center;justify-content:center;width:100%;min-height:48px;margin:8px 0 0;padding:10px 16px;border-radius:12px;border:1px solid var(--accent);background:var(--accent);color:var(--accent-fg);font:inherit;font-weight:600;cursor:pointer}
.btn.quiet{background:transparent;color:var(--accent)}
.btn.danger{background:var(--bad);border-color:var(--bad);color:#fff}
.row2{display:flex;gap:8px}.row2 form{flex:1}
form{margin:0}
fieldset{border:0;padding:0;margin:12px 0 0}
legend{font-weight:600;margin-bottom:8px;padding:0}
.opt{display:flex;gap:12px;align-items:flex-start;min-height:48px;padding:12px 14px;margin:8px 0;border:1px solid var(--line);border-radius:12px;background:var(--surface)}
.opt input{width:22px;height:22px;margin:1px 0 0;flex:none;accent-color:var(--accent)}
.opt:has(input:checked){border-color:var(--accent)}
.opt .desc{display:block;color:var(--dim);font-size:14px}
pre,code{font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
pre{margin:8px 0 0;padding:10px 12px;background:var(--code);border-radius:10px;overflow-x:auto;white-space:pre;max-width:100%}
pre.wrap-text{white-space:pre-wrap;overflow-wrap:anywhere}
.pager{display:flex;justify-content:space-between;gap:8px;margin:14px 0}
.pager a,.pager span{display:inline-flex;align-items:center;min-height:44px;padding:0 12px}
.pager span{color:var(--dim)}
.day{text-align:center;color:var(--dim);font-size:13px;margin:18px 0 6px}
.msg{display:flex;flex-direction:column;margin:8px 0;max-width:100%}
.msg .who{font-size:12px;color:var(--dim);margin:0 6px 2px}
.bubble{background:var(--bubble);border:1px solid var(--line);border-radius:16px 16px 16px 4px;padding:10px 12px;max-width:88%;min-width:0}
.bubble .p{white-space:pre-wrap;margin:0}
.bubble .p+.p,.bubble pre+.p{margin-top:8px}
.msg.you{align-items:flex-end}
.msg.you .bubble{background:var(--you);color:var(--you-fg);border-color:var(--you);border-radius:16px 16px 4px 16px}
.msg.you pre{background:rgba(0,0,0,.18);color:inherit}
.msg.peer .bubble,.msg.sent .bubble{border-left:3px solid var(--ext)}
.msg.sent{align-items:flex-end}
.msg.note{align-items:center}
.msg.note .bubble{background:transparent;border:0;color:var(--dim);font-size:13px;padding:2px 8px;text-align:center}
details summary{cursor:pointer;list-style:none}
details summary::-webkit-details-marker{display:none}
.more{color:var(--accent);font-size:14px;margin-top:6px}
details[open] .fold-summary{display:none}
.composer{position:sticky;bottom:0;background:var(--bg);border-top:1px solid var(--line);margin:16px -16px 0;padding:10px 16px calc(10px + env(safe-area-inset-bottom))}
textarea,input[type=text]{width:100%;font:inherit;font-size:16px;padding:12px;border:1px solid var(--line);border-radius:12px;background:var(--surface);color:var(--fg)}
textarea{min-height:88px;resize:vertical}
label.field{display:block;margin:14px 0 6px;font-weight:600}
.check{display:flex;gap:10px;align-items:center;min-height:44px}
.check input{width:20px;height:20px}
.actions{margin-top:14px}
.actions summary{display:flex;align-items:center;min-height:44px;color:var(--dim)}
.empty{text-align:center;color:var(--dim);padding:40px 0}
</style></head><body>
<header class="bar"><div class="wrap">
<a class="brand" href="{{$.Base}}/">ccs</a>
{{if ge .Waiting 0}}<a class="badge{{if eq .Waiting 0}} zero{{end}}" href="{{$.Base}}/">{{.Waiting}} waiting on you</a>{{end}}
<span class="updated">updated {{.Updated}}</span>
<nav><a href="{{$.Base}}/">Sessions</a><a href="{{$.Base}}/prs">PRs</a>{{if .Write}}<a href="{{$.Base}}/launch">Launch</a>{{end}}<span class="mode">{{if .Preview}}local preview, {{if .Write}}read and write{{else}}read-only{{end}}{{else if .Write}}read and write{{else}}read-only{{end}}</span></nav>
</div></header>
<main class="wrap">
{{end}}

{{define "foot"}}</main></body></html>{{end}}

{{define "csrf"}}<input type="hidden" name="csrf" value="{{.}}">{{end}}

{{define "prompt"}}{{/* one held prompt: . is {P, Base, CSRF, SID, SName} */}}
<section class="card hold"><div class="title"><span>Waiting for you here: {{.P.Tool}}</span><span class="dim">{{.P.Age}}</span></div>
{{if .P.Questions}}<form method="post" action="{{.Base}}/act">{{template "csrf" .CSRF}}
<input type="hidden" name="action" value="prompt"><input type="hidden" name="choice" value="answer"><input type="hidden" name="prompt" value="{{.P.ID}}">
<input type="hidden" name="id" value="{{.SID}}"><input type="hidden" name="name" value="{{.SName}}">
{{range .P.Questions}}{{$key := .Key}}{{$multi := .Multi}}<fieldset><legend>{{.Question}}{{if .Multi}} <span class="dim">(pick any)</span>{{end}}</legend>
{{range .Options}}<label class="opt"><input type="{{if $multi}}checkbox{{else}}radio{{end}}" name="{{$key}}" value="{{.Label}}"><span>{{.Label}}{{if .Description}}<span class="desc">{{.Description}}</span>{{end}}</span></label>{{end}}
<label class="opt"><input type="{{if $multi}}checkbox{{else}}radio{{end}}" name="{{$key}}" value="__other__"><span>Other<span class="desc">your own answer{{if $multi}}, in place of any ticked{{end}}:</span><input type="text" name="{{$key}}_other" maxlength="2000" autocomplete="off"></span></label>
</fieldset>{{end}}
<button class="btn">Answer</button></form>
{{else}}<pre class="wrap-text">{{.P.Summary}}</pre>{{if .P.Input}}<details><summary class="more">Full input</summary><pre>{{.P.Input}}</pre></details>{{end}}
<div class="row2">
<form method="post" action="{{.Base}}/act">{{template "csrf" .CSRF}}<input type="hidden" name="action" value="prompt"><input type="hidden" name="choice" value="allow"><input type="hidden" name="prompt" value="{{.P.ID}}"><input type="hidden" name="id" value="{{.SID}}"><input type="hidden" name="name" value="{{.SName}}"><button class="btn">Allow</button></form>
<form method="post" action="{{.Base}}/act">{{template "csrf" .CSRF}}<input type="hidden" name="action" value="prompt"><input type="hidden" name="choice" value="deny"><input type="hidden" name="prompt" value="{{.P.ID}}"><input type="hidden" name="id" value="{{.SID}}"><input type="hidden" name="name" value="{{.SName}}"><button class="btn danger">Deny</button></form>
</div>{{end}}
<form method="post" action="{{.Base}}/act">{{template "csrf" .CSRF}}<input type="hidden" name="action" value="prompt"><input type="hidden" name="choice" value="mac"><input type="hidden" name="prompt" value="{{.P.ID}}"><input type="hidden" name="id" value="{{.SID}}"><input type="hidden" name="name" value="{{.SName}}"><button class="btn quiet">Show it in its tab instead</button></form>
</section>{{end}}

{{define "triage"}}{{template "head" .}}
{{if .Body.Prompts}}<h2>Waiting for you here</h2>
{{range .Body.Prompts}}<a class="card hold" href="{{$.Base}}/session?id={{.Session}}"><div class="top"><span class="dot waiting_on_user"></span><span class="name">{{.Name}}</span><span class="age">{{.Age}}</span></div>
<div class="reason">{{.Tool}}{{if .Summary}} · {{.Summary}}{{end}}{{range .Questions}} · {{.Question}}{{end}}</div></a>{{end}}{{end}}
{{range .Body.Buckets}}<h2>{{.Title}} · {{len .Rows}}</h2>
{{range .Rows}}<a class="card" href="{{$.Base}}/session?id={{.Session}}">
<div class="top"><span class="dot {{.Bucket}}"></span><span class="name">{{.Name}}</span>{{if .Idle}}<span class="age">{{.Idle}}</span>{{end}}</div>
<div class="reason">{{if .Status}}{{.Status}} · {{end}}{{.Reason}}</div>
{{if .LastSaid}}<div class="said">{{.LastSaid}}</div>{{end}}
{{if .OpenPRs}}<div class="chips">{{range .OpenPRs}}<span class="chip {{.Group}}">{{prName .PR}} · {{.Status}}</span>{{end}}</div>{{end}}
</a>{{end}}{{else}}<p class="empty">No sessions.</p>{{end}}
{{template "foot"}}{{end}}

{{define "session"}}{{template "head" .}}{{$csrf := .CSRF}}{{$write := .Write}}{{$base := .Base}}{{with .Body}}
<p class="dim" style="margin:14px 0 0"><a href="{{$base}}/">← Sessions</a></p>
<h1>{{if .Name}}{{.Name}}{{else}}{{.ID}}{{end}}</h1>
<div class="dim">{{if .Live}}running{{if .Status}} · {{.Status}}{{end}}{{else}}not running{{end}}{{if .Total}} · {{.Total}} messages{{end}}</div>
{{$sid := .ID}}{{$sname := .Name}}{{range .Prompts}}{{template "prompt" (promptArgs . $base $csrf $sid $sname)}}{{end}}
{{if and .Ask (not .Prompts)}}<section class="card hold"><div class="title">Asking</div><pre class="wrap-text">{{.Ask.Question}}</pre>
<p class="dim">Answer it in its tab, at the Mac: with the ccs hook installed (ccs hook install), ccs serve holds new prompts here to answer (docs/remote.md).</p></section>{{end}}
<div class="pager">{{if .HasPrev}}<a href="{{$base}}/session?id={{.ID}}&offset={{.Prev}}">← Older</a>{{else}}<span>Start</span>{{end}}<a href="{{$base}}/session?id={{.ID}}">Latest</a>{{if .HasNext}}<a href="{{$base}}/session?id={{.ID}}&offset={{.Next}}">Newer →</a>{{else}}<span>Newest</span>{{end}}</div>
{{range .Messages}}{{if .Day}}<div class="day">{{.Day}}</div>{{end}}
<div class="msg {{.Kind}}"><div class="who">{{.Who}}{{if .Time}} · {{.Time}}{{end}}</div><div class="bubble">
{{if .Long}}<details><summary><span class="fold-summary"><span class="p">{{.Summary}}</span><span class="more">Show all {{.Lines}} lines</span></span></summary>{{template "segments" .Segments}}</details>
{{else}}{{template "segments" .Segments}}{{end}}</div></div>
{{else}}<p class="empty">No messages yet.</p>{{end}}
{{if and $write .Live .Name}}
{{if .Waiting}}<p class="dim">It's waiting on you: answer above, or in its tab, before sending a message.</p>
{{else}}<form class="composer" method="post" action="{{$base}}/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="send"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
<textarea name="text" placeholder="Message {{.Name}}" maxlength="8000" aria-label="Message {{.Name}}"></textarea>
<button class="btn">Send</button></form>{{end}}
<details class="actions"><summary>More actions</summary>
<form method="post" action="{{$base}}/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="restart"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
<button class="btn quiet">Restart</button></form>
<form method="post" action="{{$base}}/act">{{template "csrf" $csrf}}
<input type="hidden" name="action" value="close"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="name" value="{{.Name}}">
{{if not .Codex}}<label class="check"><input type="checkbox" name="graceful" checked> Let it wrap up first</label>{{end}}
<button class="btn danger">Close</button></form>
</details>
{{end}}{{end}}
{{template "foot"}}{{end}}

{{define "segments"}}{{range .}}{{if .Code}}<pre>{{.Text}}</pre>{{else}}<p class="p">{{.Text}}</p>{{end}}{{end}}{{end}}

{{define "text"}}{{template "head" .}}<h1>{{.Body.Heading}}</h1><section class="card"><pre class="wrap-text">{{.Body.Text}}</pre></section>{{template "foot"}}{{end}}

{{define "launch"}}{{template "head" .}}
<h1>Launch a session</h1>
<form class="card" method="post" action="{{$.Base}}/act">{{template "csrf" .CSRF}}
<input type="hidden" name="action" value="launch">
<label class="field" for="l-name">Name</label><input id="l-name" type="text" name="name" required autocapitalize="off" autocorrect="off">
<label class="field" for="l-cwd">Folder (absolute path)</label><input id="l-cwd" type="text" name="cwd" required autocapitalize="off" autocorrect="off">
<label class="field" for="l-brief">Brief</label><textarea id="l-brief" name="brief"></textarea>
<label class="check"><input type="checkbox" name="codex"> Codex instead of Claude Code</label>
<button class="btn">Launch</button></form>
{{template "foot"}}{{end}}

{{define "confirm"}}{{template "head" .}}{{$csrf := .CSRF}}{{with .Body}}
<h1>Confirm</h1>
<section class="card"><pre class="wrap-text">{{.What}}</pre>
<form method="post" action="{{$.Base}}/act">{{template "csrf" $csrf}}<input type="hidden" name="confirm" value="yes">
<input type="hidden" name="action" value="{{.A.Kind}}"><input type="hidden" name="id" value="{{.A.ID}}"><input type="hidden" name="name" value="{{.A.Name}}">
<input type="hidden" name="text" value="{{.A.Text}}"><input type="hidden" name="prompt" value="{{.A.Prompt}}"><input type="hidden" name="choice" value="{{.A.Choice}}">{{range .A.AnswerFields}}<input type="hidden" name="{{index . 0}}" value="{{index . 1}}">{{end}}
<input type="hidden" name="cwd" value="{{.A.Cwd}}"><input type="hidden" name="brief" value="{{.A.Brief}}">
{{if .A.Codex}}<input type="hidden" name="codex" value="on">{{end}}{{if .A.Graceful}}<input type="hidden" name="graceful" value="on">{{end}}
<button class="btn">Yes, do it</button></form>
<p class="pager">{{if .A.ID}}<a href="{{$.Base}}/session?id={{.A.ID}}">Cancel</a>{{else}}<a href="{{$.Base}}/">Cancel</a>{{end}}</p></section>
{{end}}{{template "foot"}}{{end}}

{{define "message"}}{{template "head" .}}{{with .Body}}<h1>{{.Heading}}</h1><section class="card"><pre class="wrap-text">{{.Text}}</pre>
<p class="pager">{{if .Back}}<a class="btn quiet" href="{{$.Base}}/session?id={{.Back}}">Back to the session</a>{{else}}<a class="btn quiet" href="{{$.Base}}/">Back to sessions</a>{{end}}</p></section>{{end}}{{template "foot"}}{{end}}
`
