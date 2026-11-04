package us.peerb.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.material3.minimumInteractiveComponentSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import us.peerb.app.Core
import us.peerb.app.Held
import us.peerb.app.Section
import us.peerb.app.Sess
import us.peerb.app.Snapshot
import us.peerb.app.ago
import us.peerb.app.plural

enum class Tab { Inbox, History, Unit }

/** The TUI's header: plate, tabs, who and how many, and the link light. */
@Composable
fun Header(tab: Tab, onTab: (Tab) -> Unit, snap: Snapshot?, problem: String?) {
    val c = LocalCasing.current
    // Beige up under the status bar, edge to edge.
    Column(Modifier.fillMaxWidth().background(Pal.beige).statusBarsPadding()) {
        Row(Modifier.fillMaxWidth().padding(start = 14.dp, end = 14.dp, top = 10.dp), verticalAlignment = Alignment.CenterVertically) {
            Hazard(Pal.ink, Modifier.width(32.dp).height(20.dp), band = 4.dp, ground = Pal.beige)
            Text("PEERBUS", style = Type.brand, color = Pal.ink, modifier = Modifier.padding(start = 8.dp))
            Spacer(Modifier.weight(1f))
            val live = snap?.live == true && problem == null
            Pulse(if (problem != null) Pal.red else if (live) Pal.teal else Pal.mustard, on = live)
            Text(
                if (problem != null) " FAULT" else if (live) " LIVE" else " LINKING",
                style = Type.label,
                color = Pal.ink,
            )
        }
        Row(Modifier.fillMaxWidth().padding(horizontal = 10.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
            for (t in Tab.entries) {
                val on = t == tab
                Text(
                    t.name.uppercase(),
                    style = Type.label,
                    color = if (on) Pal.beige else Pal.ink,
                    modifier = Modifier
                        .padding(end = 6.dp)
                        .background(if (on) Pal.ink else Color.Transparent, RoundedCornerShape(2.dp))
                        .minimumInteractiveComponentSize()
                        .clickable { onTab(t) }
                        .padding(horizontal = 14.dp, vertical = 10.dp),
                )
            }
            Spacer(Modifier.weight(1f))
            if (snap != null) {
                val who = snap.self.ifEmpty { "you" }
                Text(
                    "$who · ${plural(snap.machines, "machine")} · ${plural(snap.sessions.size, "session")}",
                    style = Type.monoSmall,
                    color = Pal.ink.copy(alpha = 0.7f),
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
        Stripe()
    }
    if (problem != null) {
        Text(
            problem,
            color = Pal.red,
            style = Type.monoSmall,
            modifier = Modifier.fillMaxWidth().background(c.panel).padding(horizontal = 14.dp, vertical = 6.dp),
        )
    }
}

/** What happened last, as the TUI's status line. */
data class Status(val text: String, val bad: Boolean, val at: Long = System.nanoTime())

@Composable
fun StatusLine(status: Status?) {
    var shown by remember { mutableStateOf<Status?>(null) }
    LaunchedEffect(status) {
        shown = status
        delay(6000)
        shown = null
    }
    val s = shown ?: return
    Text(
        s.text,
        color = if (s.bad) Pal.red else Pal.teal,
        style = Type.mono,
        fontWeight = FontWeight.Bold,
        modifier = Modifier.fillMaxWidth().background(Pal.screen).padding(horizontal = 16.dp, vertical = 14.dp),
    )
}

@Composable
fun InboxPage(snap: Snapshot?, open: String?, onOpen: (String?) -> Unit, onStatus: (Status) -> Unit, onWrite: (Draft) -> Unit) {
    val item = open?.let { id -> snap?.find(id) }
    if (snap != null && item != null) {
        BackHandler { onOpen(null) }
        Detail(snap, item, onClose = { onOpen(null) }, onStatus = onStatus, onWrite = onWrite)
        return
    }
    if (open != null && snap?.live == true) {
        // It was dealt with elsewhere (or here) and is gone.
        LaunchedEffect(open) { onOpen(null) }
    }
    Box(Modifier.fillMaxSize()) {
        if (snap == null || snap.items.isEmpty()) Quiet(snap) else HeldList(snap, onOpen)
        if (snap?.live == true) WriteKey({ onWrite(Draft()) }, Modifier.align(Alignment.BottomEnd))
    }
}

@Composable
private fun HeldList(snap: Snapshot, onOpen: (String?) -> Unit) {
    val c = LocalCasing.current
    LazyColumn(Modifier.fillMaxSize(), contentPadding = androidx.compose.foundation.layout.PaddingValues(top = 8.dp, bottom = 96.dp)) {
        for (section in Section.entries) {
            val rows = snap.items.filter { it.section == section }
            if (rows.isEmpty()) continue
            item(key = "s-$section") {
                SectionLabel(section.title, Pal.lanes[section.ordinal], rows.size, Modifier.padding(start = 14.dp, end = 14.dp, top = 14.dp, bottom = 8.dp))
            }
            items(rows, key = { it.id }) { h ->
                Panel(Pal.lanes[section.ordinal], Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 5.dp).clickable { onOpen(h.id) }) {
                    Column {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(who(h, snap.self), style = Type.mono, fontWeight = FontWeight.Bold, color = c.fg, modifier = Modifier.weight(1f), maxLines = 1, overflow = TextOverflow.Ellipsis)
                            if (h.fake) Plate("test", Modifier.padding(end = 8.dp), bg = Pal.mustard)
                            Text(ago(h.heldAt), style = Type.monoSmall, color = c.muted)
                        }
                        Text(
                            h.text.lineSequence().first(),
                            style = Type.body,
                            color = c.muted,
                            maxLines = 2,
                            overflow = TextOverflow.Ellipsis,
                            modifier = Modifier.padding(top = 4.dp),
                        )
                    }
                }
            }
        }
    }
}

private fun who(h: Held, self: String) = when (h.section) {
    Section.ToRead -> "${short(h.from, self)} → you"
    Section.ForSession -> "${h.peer} → ${h.targetLabel}"
    Section.ForYou -> "${h.peer} → you"
    Section.Yours -> "${h.targetLabel} → ${h.peer}"
}

@Composable
private fun Quiet(snap: Snapshot?) {
    Box(Modifier.fillMaxSize().padding(16.dp), contentAlignment = Alignment.Center) {
        PhosphorScreen(Modifier.fillMaxWidth()) {
            when {
                snap == null || !snap.live -> {
                    Text("> LINKING TO YOUR MACHINES", style = Type.screen)
                    Row { Text("  ", style = Type.screen); Cursor() }
                }
                snap.sessions.isEmpty() -> {
                    Text("> NO SESSIONS ONLINE", style = Type.screen)
                    Text("Held messages show up while a peerbus session runs on the machine holding them.", style = Type.screenNote, modifier = Modifier.padding(top = 8.dp))
                }
                else -> {
                    Text("> NOTHING HELD", style = Type.screen)
                    Text("all quiet on ${plural(snap.machines, "machine")}", style = Type.screenNote, modifier = Modifier.padding(top = 4.dp))
                    Row(Modifier.padding(top = 4.dp)) { Text("> ", style = Type.screen); Cursor() }
                }
            }
        }
    }
}

private enum class Mode { Browsing, Routing, ConfirmingDeny }

@Composable
private fun Detail(snap: Snapshot, h: Held, onClose: () -> Unit, onStatus: (Status) -> Unit, onWrite: (Draft) -> Unit) {
    val c = LocalCasing.current
    val scope = rememberCoroutineScope()
    var mode by remember(h.id) { mutableStateOf(Mode.Browsing) }
    var busy by remember(h.id) { mutableStateOf(false) }

    fun act(what: suspend () -> String) {
        if (busy) return
        busy = true
        scope.launch {
            try {
                onStatus(Status(what(), false))
                onClose()
            } catch (e: Exception) {
                onStatus(Status(e.message ?: "failed", true))
            } finally {
                busy = false
                mode = Mode.Browsing
            }
        }
    }

    if (mode == Mode.Routing) {
        BackHandler { mode = Mode.Browsing }
        Route(snap, h, busy, onCancel = { mode = Mode.Browsing }) { s -> act { Core.approve(h.id, s.sid) } }
        return
    }

    Column(Modifier.fillMaxSize()) {
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                BackLink(onClose)
                Spacer(Modifier.weight(1f))
                SectionLabel(h.section.title, Pal.lanes[h.section.ordinal])
            }
            Spacer(Modifier.height(14.dp))
            when (h.section) {
                Section.ToRead -> {
                    Field("from", h.from)
                    Field("to", "you", note = "to read; reply or dismiss")
                }
                Section.ForSession -> {
                    Field("from", h.from)
                    Field("to", h.to, note = h.targetLabel)
                }
                Section.ForYou -> {
                    Field("from", h.from)
                    Field("to", "you, as a person", note = "pick a session")
                }
                Section.Yours -> {
                    Field("from", h.from)
                    Field("to", h.to, note = "held by your send=ask setting for ${h.peer}")
                }
            }
            Field("held", "on ${h.machine} · ${ago(h.heldAt)} ago", note = "thread ${h.thread}")
            if (h.fake) Field("", "a test message from peerbus fake", note = "approving it delivers nothing")
            Spacer(Modifier.height(14.dp))
            PhosphorScreen(Modifier.fillMaxWidth()) {
                SelectionContainer { Text(h.text, style = Type.screen) }
            }
        }
        Stripe(height = 3.dp)
        Row(
            Modifier.fillMaxWidth().background(c.panel).padding(horizontal = 12.dp, vertical = 12.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            when (h.section) {
                Section.ToRead -> Keycap("Reply", { onClose(); onWrite(Draft(h.from, h.thread, h.msgId)) }, Modifier.weight(1.4f), key = "R", face = Pal.teal, enabled = !busy)
                Section.ForYou -> Keycap("Pick session", { mode = Mode.Routing }, Modifier.weight(1.4f), key = "A", face = Pal.mustard, enabled = !busy)
                Section.Yours -> Keycap("Send", { act { Core.approve(h.id) } }, Modifier.weight(1f), key = "A", face = Pal.teal, enabled = !busy)
                Section.ForSession -> Keycap("Approve", { act { Core.approve(h.id) } }, Modifier.weight(1.2f), key = "A", face = Pal.teal, enabled = !busy)
            }
            if (h.section == Section.ForSession) {
                Keycap("Route", { mode = Mode.Routing }, Modifier.weight(1f), key = "R", enabled = !busy)
            }
            Keycap(if (h.note) "Dismiss" else "Decline", { mode = Mode.ConfirmingDeny }, Modifier.weight(1.1f), key = "D", face = Pal.red, enabled = !busy)
        }
    }

    if (mode == Mode.ConfirmingDeny) {
        val verb = if (h.note) "dismiss" else "decline"
        Dialog(onDismissRequest = { mode = Mode.Browsing; onStatus(Status("not ${verb}d", false)) }) {
            Column(
                Modifier
                    .background(c.panel, RoundedCornerShape(6.dp))
                    .border(2.dp, Pal.ink, RoundedCornerShape(6.dp))
                    .padding(18.dp),
            ) {
                SectionLabel(verb, Pal.red)
                Text("${verb.replaceFirstChar { it.uppercase() }} this message?", style = Type.title, color = c.fg, modifier = Modifier.padding(top = 14.dp))
                Text(
                    when {
                        h.note -> "It's gone from your inbox; the sender isn't told."
                        h.fake -> "It's a test; nobody is told."
                        h.section == Section.Yours -> "It won't be sent."
                        else -> "${h.peer} is told."
                    },
                    color = c.muted,
                    modifier = Modifier.padding(top = 4.dp, bottom = 18.dp),
                )
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Keycap("No", { mode = Mode.Browsing; onStatus(Status("not ${verb}d", false)) }, Modifier.weight(1f), key = "N")
                    Keycap("Yes", { act { Core.deny(h.id) } }, Modifier.weight(1f), key = "Y", face = Pal.red, enabled = !busy)
                }
            }
        }
    }
}

/** Pick one of your sessions, on any machine, to deliver to: the TUI's ROUTE TO. */
@Composable
private fun Route(snap: Snapshot, h: Held, busy: Boolean, onCancel: () -> Unit, onPick: (Sess) -> Unit) {
    val c = LocalCasing.current
    var pick by remember(h.id) { mutableStateOf(snap.sessions.firstOrNull { it.sid == h.target } ?: snap.sessions.firstOrNull()) }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                BackLink(onCancel)
                Spacer(Modifier.weight(1f))
                SectionLabel("route to", Pal.orange)
            }
            Text("from ${h.from}", style = Type.monoSmall, color = c.muted, modifier = Modifier.padding(top = 10.dp))
        }
        if (snap.sessions.isEmpty()) {
            Text("None of your sessions are online to route to.", color = Pal.red, modifier = Modifier.padding(16.dp))
        }
        LazyColumn(Modifier.weight(1f)) {
            var host = ""
            for (s in snap.sessions) {
                if (s.host != host) {
                    host = s.host
                    item(key = "h-$host") {
                        Text(host.uppercase(), style = Type.label, color = c.muted, modifier = Modifier.padding(start = 16.dp, top = 12.dp, bottom = 4.dp))
                    }
                }
                item(key = s.sid) {
                    val on = s == pick
                    Row(
                        Modifier
                            .fillMaxWidth()
                            .padding(horizontal = 12.dp, vertical = 2.dp)
                            .background(if (on) Pal.orange else Color.Transparent, RoundedCornerShape(3.dp))
                            .clickable { pick = s }
                            .padding(horizontal = 12.dp, vertical = 16.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        val fg = if (on) Pal.ink else c.fg
                        Text(s.name, fontFamily = Mono, fontWeight = FontWeight.Bold, color = fg, modifier = Modifier.widthIn(min = 120.dp).padding(end = 10.dp), maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Text(s.status, style = Type.monoSmall, color = if (s.status == "idle") (if (on) Pal.ink else Pal.teal) else (if (on) Pal.ink else Pal.mustard), modifier = Modifier.widthIn(min = 56.dp).padding(end = 10.dp))
                        Text(if (s.channels != "on") "channels unverified  ${s.cwd}" else s.cwd, style = Type.monoSmall, color = if (on) Pal.ink else c.muted, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                }
            }
        }
        Stripe(height = 3.dp)
        Row(Modifier.fillMaxWidth().background(c.panel).padding(12.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Keycap("Cancel", onCancel, Modifier.weight(1f), key = "Q")
            val p = pick
            Keycap(
                if (p != null) "Deliver to ${p.name}" else "Deliver",
                { if (p != null) onPick(p) },
                Modifier.weight(2f),
                key = "↵",
                face = Pal.teal,
                enabled = p != null && !busy,
            )
        }
    }
}
