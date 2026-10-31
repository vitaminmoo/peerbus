package us.peerb.app.ui

import androidx.activity.compose.BackHandler
import androidx.compose.foundation.background
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
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay
import us.peerb.app.Core
import us.peerb.app.HUMAN
import us.peerb.app.Record
import us.peerb.app.ago

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

/** Every message your sessions sent and received, on every machine: the TUI's history page. */
@Composable
fun HistoryPage(self: String, onWrite: (Draft) -> Unit) {
    var records by remember { mutableStateOf<List<Record>?>(null) }
    var problem by remember { mutableStateOf<String?>(null) }
    var filter by rememberSaveable { mutableStateOf(Filter.All) }
    var open by rememberSaveable { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) {
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

    val rec = open?.let { k -> records?.firstOrNull { it.key == k } }
    if (rec != null) {
        BackHandler { open = null }
        HistoryDetail(rec, self, onClose = { open = null }, onReply = { onWrite(Draft(replyTo(rec, self), rec.thread)) })
        return
    }

    val c = LocalCasing.current
    // Newest first, ties broken by key, so a refresh never reshuffles them.
    val shown = records.orEmpty().filter { filter.keeps(it) }.sortedWith(compareByDescending<Record> { it.at }.thenBy { it.key })
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
                    items(shown, key = { it.key }) { r ->
                        Panel(statusColour(r.status), Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 5.dp).clickable { open = r.key }) {
                            Column {
                                Row(verticalAlignment = Alignment.CenterVertically) {
                                    Text("${short(r.from, self)} → ${short(r.to, self)}", style = Type.mono, fontWeight = FontWeight.Bold, color = c.fg, modifier = Modifier.weight(1f), maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    Text(ago(r.at), style = Type.monoSmall, color = c.muted)
                                }
                                Text(statusText(r, self), style = Type.monoSmall, color = statusColour(r.status), modifier = Modifier.padding(top = 2.dp))
                                Text(r.text.lineSequence().first(), style = Type.body, color = c.muted, maxLines = 2, overflow = TextOverflow.Ellipsis, modifier = Modifier.padding(top = 4.dp))
                            }
                        }
                    }
                }
            }
            WriteKey({ onWrite(Draft()) }, Modifier.align(Alignment.BottomEnd))
        }
    }
}

@Composable
private fun HistoryDetail(r: Record, self: String, onClose: () -> Unit, onReply: () -> Unit) {
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
            Spacer(Modifier.height(14.dp))
            PhosphorScreen(Modifier.fillMaxWidth()) {
                SelectionContainer { Text(text, style = Type.screen) }
                more?.let { Text(it, style = Type.screenNote, modifier = Modifier.padding(top = 6.dp)) }
            }
        }
        Stripe(height = 3.dp)
        Row(Modifier.fillMaxWidth().background(c.panel).padding(12.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Keycap("Reply to ${short(replyTo(r, self), self)}", onReply, Modifier.fillMaxWidth(), key = "R", face = Pal.teal)
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
