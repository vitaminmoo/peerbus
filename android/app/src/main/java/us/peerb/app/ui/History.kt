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
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.material3.minimumInteractiveComponentSize
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import us.peerb.app.Core
import us.peerb.app.HUMAN
import us.peerb.app.Record
import us.peerb.app.ago
import us.peerb.app.plural

/** Which messages the history shows, as the TUI's f key cycles them. */
private enum class Filter(val title: String) {
    All("all"),
    Own("yours"),
    Friends("friends");

    fun keeps(r: Record) = when (this) {
        All -> true
        Own -> r.peer.isEmpty()
        Friends -> r.peer.isNotEmpty()
    }
}

/** Newest first, ties broken by key, so a refresh never reshuffles them. */
private val newestFirst = compareByDescending<Record> { it.at }.thenBy { it.key }

/**
 * Every conversation your sessions had, on every machine, and the messages
 * of the one opened: the TUI's history page.
 */
@Composable
fun HistoryPage(self: String, onWrite: (Draft) -> Unit, onStatus: (Status) -> Unit) {
    var records by remember { mutableStateOf<List<Record>?>(null) }
    var problem by remember { mutableStateOf<String?>(null) }
    var filter by rememberSaveable { mutableStateOf(Filter.All) }
    var thread by rememberSaveable { mutableStateOf<String?>(null) }
    var open by rememberSaveable { mutableStateOf<String?>(null) }
    var deleting by remember { mutableStateOf<Record?>(null) }
    var busy by remember { mutableStateOf(false) }
    // Bumped by a deletion, so a fetch that began before it can't bring it back.
    var generation by remember { mutableStateOf(0) }
    val scope = rememberCoroutineScope()
    LaunchedEffect(generation) {
        while (true) {
            try {
                records = Core.history()
                problem = null
            } catch (e: Exception) {
                problem = e.message
            }
            delay(3000)
        }
    }

    val all = records.orEmpty()
    val confirm = deleting
    if (confirm != null) {
        val n = all.count { it.threadKey == confirm.threadKey }
        DeleteThreadDialog(n, busy, onNo = { deleting = null; onStatus(Status("not deleted", false)) }) {
            busy = true
            scope.launch {
                try {
                    onStatus(Status(Core.deleteThread(confirm.thread), false))
                    generation++
                    records = records?.filterNot { it.threadKey == confirm.threadKey }
                    open = null
                    thread = null
                } catch (e: Exception) {
                    onStatus(Status(e.message ?: "couldn't delete the thread", true))
                } finally {
                    busy = false
                    deleting = null
                }
            }
        }
    }

    val rec = open?.let { k -> all.firstOrNull { it.key == k } }
    if (rec != null) {
        BackHandler { open = null }
        HistoryDetail(
            rec,
            self,
            onClose = { open = null },
            onReply = { onWrite(Draft(replyTo(rec, self), rec.thread, rec.id)) },
            onDelete = { deleting = rec },
        )
        return
    }

    val inThread = thread?.let { t -> all.filter { it.threadKey == t }.sortedWith(newestFirst.reversed()) }.orEmpty()
    if (thread != null && records != null && inThread.isEmpty()) SideEffect { thread = null } // deleted, perhaps from elsewhere
    if (thread != null && inThread.isNotEmpty()) {
        BackHandler { thread = null }
        ThreadPage(inThread, self, onClose = { thread = null }, onOpen = { open = it.key }, onDelete = { deleting = inThread.last() })
        return
    }

    val c = LocalCasing.current
    // Each conversation, by its newest message under the filter.
    val shown = all.filter { filter.keeps(it) }.sortedWith(newestFirst).distinctBy { it.threadKey }
    val counts = all.groupingBy { it.threadKey }.eachCount()
    // A list per filter: switching starts at the top rather than anchored
    // to whatever message was on screen under the last one.
    val list = remember(filter) { LazyListState() }
    val newest = shown.firstOrNull()?.key
    LaunchedEffect(newest) {
        if (list.firstVisibleItemIndex <= 1) list.scrollToItem(0) // keep new arrivals in view at the top
    }
    Column(Modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 10.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
            for (f in Filter.entries) {
                val on = f == filter
                Text(
                    f.title.uppercase(),
                    style = Type.label,
                    color = if (on) Pal.ink else c.muted,
                    modifier = Modifier
                        .padding(end = 6.dp)
                        .minimumInteractiveComponentSize()
                        .background(if (on) Pal.beige else Color.Transparent, RoundedCornerShape(2.dp))
                        .clickable { filter = f }
                        .padding(horizontal = 12.dp, vertical = 8.dp),
                )
            }
            Spacer(Modifier.weight(1f))
            Text("${shown.size}", style = Type.monoSmall, color = c.muted)
        }
        problem?.let { Text(it, color = Pal.red, style = Type.monoSmall, modifier = Modifier.padding(horizontal = 16.dp)) }
        Box(Modifier.weight(1f)) {
            when {
                records == null -> Box(Modifier.fillMaxSize().padding(16.dp), contentAlignment = Alignment.Center) {
                    PhosphorScreen(Modifier.fillMaxWidth()) {
                        Text("> READING THE LOGS", style = Type.screen)
                        Row { Text("  ", style = Type.screen); Cursor() }
                    }
                }
                shown.isEmpty() -> Box(Modifier.fillMaxSize().padding(16.dp), contentAlignment = Alignment.Center) {
                    PhosphorScreen(Modifier.fillMaxWidth()) {
                        Text("> NO MESSAGES", style = Type.screen)
                        Text("in the last 30 days, on machines with a session online", style = Type.screenNote, modifier = Modifier.padding(top = 4.dp))
                    }
                }
                else -> LazyColumn(Modifier.fillMaxSize(), state = list, contentPadding = androidx.compose.foundation.layout.PaddingValues(top = 4.dp, bottom = 96.dp)) {
                    items(shown, key = { it.threadKey }) { r ->
                        RecordRow(r, self, count = counts[r.threadKey] ?: 1) { thread = r.threadKey }
                    }
                }
            }
            WriteKey({ onWrite(Draft()) }, Modifier.align(Alignment.BottomEnd))
        }
    }
}

@Composable
private fun RecordRow(r: Record, self: String, count: Int, onClick: () -> Unit) {
    val c = LocalCasing.current
    Panel(statusColour(r.status), Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 5.dp).clickable(onClick = onClick)) {
        Column {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("${short(r.from, self)} → ${short(r.to, self)}", style = Type.mono, fontWeight = FontWeight.Bold, color = c.fg, modifier = Modifier.weight(1f), maxLines = 1, overflow = TextOverflow.Ellipsis)
                if (count > 1) Text(plural(count, "message"), style = Type.monoSmall, color = c.muted, modifier = Modifier.padding(horizontal = 8.dp))
                Text(ago(r.at), style = Type.monoSmall, color = c.muted)
            }
            Text(statusText(r, self), style = Type.monoSmall, color = statusColour(r.status), modifier = Modifier.padding(top = 2.dp))
            Text(r.text.lineSequence().first(), style = Type.body, color = c.muted, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.padding(top = 4.dp))
        }
    }
}

/** One conversation's messages, oldest first. */
@Composable
private fun ThreadPage(messages: List<Record>, self: String, onClose: () -> Unit, onOpen: (Record) -> Unit, onDelete: () -> Unit) {
    val c = LocalCasing.current
    Column(Modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = Alignment.CenterVertically) {
            BackLink(onClose)
            Spacer(Modifier.weight(1f))
            SectionLabel("thread · ${plural(messages.size, "message")}", Pal.teal)
        }
        LazyColumn(Modifier.weight(1f)) {
            items(messages, key = { it.key }) { r -> RecordRow(r, self, count = 1) { onOpen(r) } }
        }
        Stripe(height = 3.dp)
        Row(Modifier.fillMaxWidth().background(c.panel).padding(12.dp)) {
            Keycap("Delete thread", onDelete, Modifier.fillMaxWidth(), key = "D", face = Pal.red)
        }
    }
}

@Composable
private fun DeleteThreadDialog(messages: Int, busy: Boolean, onNo: () -> Unit, onYes: () -> Unit) {
    val c = LocalCasing.current
    Dialog(onDismissRequest = onNo) {
        Column(
            Modifier
                .background(c.panel, RoundedCornerShape(6.dp))
                .border(2.dp, Pal.ink, RoundedCornerShape(6.dp))
                .padding(18.dp),
        ) {
            SectionLabel("delete", Pal.red)
            Text("Delete this thread?", style = Type.title, color = c.fg, modifier = Modifier.padding(top = 14.dp))
            Text(
                "${plural(messages, "message")} go from all your machines. Messages waiting for you to approve stay in your inbox. Nobody else is told, and their copies stay.",
                color = c.muted,
                modifier = Modifier.padding(top = 4.dp, bottom = 18.dp),
            )
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Keycap("No", onNo, Modifier.weight(1f), key = "N")
                Keycap("Yes", onYes, Modifier.weight(1f), key = "Y", face = Pal.red, enabled = !busy)
            }
        }
    }
}

@Composable
private fun HistoryDetail(r: Record, self: String, onClose: () -> Unit, onReply: () -> Unit, onDelete: () -> Unit) {
    val c = LocalCasing.current
    var text by remember(r.key) { mutableStateOf(r.text) }
    var more by remember(r.key) { mutableStateOf<String?>(null) }
    LaunchedEffect(r.key) {
        if (r.cut) {
            more = "loading the rest…"
            more = try {
                text = Core.fullText(r.key)
                null
            } catch (e: Exception) {
                "couldn't load the rest: ${e.message}"
            }
        }
    }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                BackLink(onClose)
                Spacer(Modifier.weight(1f))
                SectionLabel("message", statusColour(r.status))
            }
            Spacer(Modifier.height(14.dp))
            Field("from", r.from)
            Field("to", r.to)
            Field("status", statusText(r, self))
            Field("sent", "${ago(r.at)} ago", note = "thread ${r.thread} · logged on ${r.machines.joinToString(", ")}")
            if (r.replyTo.isNotEmpty()) Field("reply", "to message ${r.replyTo}")
            Spacer(Modifier.height(14.dp))
            PhosphorScreen(Modifier.fillMaxWidth()) {
                SelectionContainer { Text(text, style = Type.screen) }
                more?.let { Text(it, style = Type.screenNote, modifier = Modifier.padding(top = 6.dp)) }
            }
        }
        Stripe(height = 3.dp)
        Row(Modifier.fillMaxWidth().background(c.panel).padding(12.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Keycap("Reply to ${short(replyTo(r, self), self)}", onReply, Modifier.weight(2f), key = "R", face = Pal.teal)
            Keycap("Delete thread", onDelete, Modifier.weight(1f), key = "D", face = Pal.red)
        }
    }
}

/** Who a reply goes to: whoever isn't you. */
private fun replyTo(r: Record, self: String) = if (r.from.startsWith("$self/")) r.to else r.from

private fun outbound(r: Record, self: String) = r.from.startsWith("$self/")

/** What became of a message, from where you sit: the TUI's wording. */
private fun statusText(r: Record, self: String): String {
    val out = outbound(r, self)
    val s = when (r.status) {
        "delivered" -> if (r.to == "$self/$HUMAN") "delivered to you" else "delivered"
        "held" -> if (out) "held · waiting for ${r.peer} to approve it" else "held · waiting in your inbox"
        "ask" -> "not sent · waiting for your go-ahead (send=ask)"
        "declined" -> if (out) "declined by ${r.peer}" else "declined by you"
        "unsent" -> "not sent · you declined it"
        else -> r.status
    }
    return if (r.fake) "$s (test)" else s
}

private fun statusColour(status: String) = when (status) {
    "delivered" -> Pal.teal
    "declined", "unsent" -> Pal.red
    else -> Pal.mustard
}
