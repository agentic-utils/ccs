package tui

import (
	"hash/maphash"
	"maps"
	"slices"
	"strconv"
	"time"
)

// Bubbletea calls View after every message: four fast ticks a second while
// a live session is selected, each live tick and held poll. Most of them
// change nothing on screen, and View draws every row of the sidebar and the
// preview. frameCache keeps the last frame, and View hands it back while
// nothing it shows can have changed:
//
//   - viewGen, which Update moves on for every message except one of the
//     few that are often no-ops (stillCandidate) and, judged by driftSig,
//     changed nothing View draws;
//   - the size;
//   - frameClock, the time as finely as anything on screen shows it: the
//     minute ("5m ago", the refresh note), or a quarter second while
//     something counts seconds or animates (the working line's spinner and
//     elapsed time, a running ! command, an update's progress, a flashing
//     gauge, the waits the bottom pane times).
//
// Nil (a model not built by Main, as in most tests) never caches.
type frameCache struct {
	key   frameKey
	frame string
	draws int // frames drawn, not handed back (tests)
}

type frameKey struct {
	gen           uint64
	width, height int
	clock         int64
}

// frameClock is now in the units frames change in: minutes, or quarter
// seconds while fastClock.
func (m model) frameClock(now time.Time) int64 {
	if m.fastClock(now) {
		return now.UnixMilli() / int64(chatFastRefresh/time.Millisecond)
	}
	return now.Unix() / 60
}

// fastClock: something on screen changes with the time faster than a
// minute.
func (m model) fastClock(now time.Time) bool {
	if m.progress != nil || (m.run != nil && m.run.running) || m.table || len(m.pending) > 0 {
		return true // update progress, the run's seconds, the table's flashing CTX, heldTimeout
	}
	if id := m.selectedSession(); id != "" && m.chatStatus[id].Status == "busy" {
		return true // the working line
	}
	for _, l := range m.allowance {
		if _, flash := gaugeColour(min(max(l.Percent, 0), 100)); flash {
			return true
		}
	}
	// The bottom pane hides a question for heldGrace after its wait began
	// or its held copy went, and up to heldAnsweredMax after ccs answered it.
	recent := func(ts map[string]time.Time) bool {
		for _, t := range ts {
			if now.Sub(t) < heldGrace+time.Second {
				return true
			}
		}
		return false
	}
	return recent(m.waitSeen) || recent(m.heldGone) || len(m.heldAnswered) > 0
}

// cachedView is View through m.frame.
func (m model) cachedView(draw func() string) string {
	if m.frame == nil {
		return draw()
	}
	key := frameKey{gen: m.viewGen, width: m.width, height: m.height, clock: m.frameClock(frameNow())}
	if m.frame.draws > 0 && m.frame.key == key {
		return m.frame.frame
	}
	m.frame.key, m.frame.frame = key, draw()
	m.frame.draws++
	return m.frame.frame
}

// stillCandidate: msg is one of the frequent messages that often change
// nothing (the live tick and what it brings back, the held poll, ccs serve's
// status), so Update checks driftSig before and after it rather than redraw.
// Anything else, every key and mouse event among them, always redraws.
func stillCandidate(msg any) bool {
	switch msg.(type) {
	case liveTickMsg, liveMsg, heldMsg, heldRetryMsg, serveStatusMsg:
		return true
	}
	return false
}

var driftSeed = maphash.MakeSeed()

// driftSig sums up everything a live tick, held poll or Update's own steps
// after them can change that View draws or the list's order depends on. Two
// equal sums mean the frame and the order are as they were. Anything the
// view reads that isn't here must only change through a message that isn't
// a stillCandidate (TestStillMessagesDrawAsAFreshView checks the ones that
// are).
func (m model) driftSig() uint64 {
	var h maphash.Hash
	h.SetSeed(driftSeed)
	str := func(s string) { h.WriteString(s); h.WriteByte(0) }
	num := func(n int64) { h.WriteString(strconv.FormatInt(n, 10)); h.WriteByte(0) }
	flag := func(b bool) {
		if b {
			h.WriteByte(1)
		} else {
			h.WriteByte(0)
		}
	}
	when := func(t time.Time) { num(t.UnixNano()) }

	// What the handlers change.
	num(int64(m.listGen))
	num(int64(len(m.items)))
	for _, id := range slices.Sorted(maps.Keys(m.live)) {
		str(id)
		flag(m.live[id])
		num(int64(m.livePIDs[id]))
		flag(m.unverified[id])
		str(m.hosts[id])
		ls := m.lastScan[id]
		num(int64(ls.PID))
		str(ls.Start)
	}
	num(int64(len(m.livePIDs)))
	num(int64(len(m.lastScan)))
	for _, id := range slices.Sorted(maps.Keys(m.bg)) {
		str(id)
		str(m.bg[id].ID)
		str(m.bg[id].State)
	}
	for _, id := range slices.Sorted(maps.Keys(m.chatStatus)) {
		st := m.chatStatus[id]
		str(id)
		str(st.Status)
		str(st.WaitingFor)
		when(st.Since)
		str(st.Name)
		str(st.Cwd)
		flag(st.Remote)
	}
	for _, ts := range []map[string]time.Time{m.waitSeen, m.heldGone, m.opened, m.sentAt} {
		h.WriteByte('|')
		for _, id := range slices.Sorted(maps.Keys(ts)) {
			str(id)
			when(ts[id])
		}
	}
	for _, id := range slices.Sorted(maps.Keys(m.heldAnswered)) {
		str(id)
		when(m.heldAnswered[id].since)
		when(m.heldAnswered[id].at)
	}
	for _, id := range slices.Sorted(maps.Keys(m.heldPrompt)) {
		p := m.heldPrompt[id]
		str(id)
		str(p.ID)
		when(p.At)
	}
	for _, id := range slices.Sorted(maps.Keys(m.pending)) {
		str(id)
		str(m.pending[id])
		flag(m.typed[id])
	}
	for _, id := range slices.Sorted(maps.Keys(m.subagents)) {
		s := m.subagents[id]
		str(id)
		str(s.Name)
		when(s.Started)
		when(s.LastWrite)
		flag(s.Finished)
	}
	flag(m.heldPrompt == nil)
	flag(m.heldBusy)
	flag(m.heldConfirmed != nil)
	str(m.heldNote)
	str(m.heldErr)
	flag(m.heldNoteShown)
	flag(m.serve.Installed)
	flag(m.serve.Loaded)
	flag(m.serve.Running)
	num(int64(m.serve.PID))
	when(m.refreshStarted)
	str(m.orchID)
	when(m.launchMod)
	str(m.errorMsg)
	str(m.noteMsg)

	// What Update's own steps after the handler change.
	num(int64(m.cursor))
	num(int64(len(m.filtered)))
	str(m.selectedID)
	str(m.viewing)
	str(m.textInput.Value())
	str(m.chatInput.Value())
	str(m.chatFor)
	flag(m.chatFocus)
	flag(m.heldFocus)
	when(m.heldAutoAt)
	when(m.autoSelectAt)
	str(m.menuShown)
	str(m.menuLeft)
	str(m.chatAfter)
	str(m.chatAfterID)
	flag(m.showUsage)
	flag(m.helpOpen)
	for _, p := range m.overPopups() {
		flag(p.open)
	}
	return h.Sum64()
}

// frameNow is the clock frames are keyed by; tests stop it.
var frameNow = time.Now
