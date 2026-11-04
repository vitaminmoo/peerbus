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
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import us.peerb.app.Core
import us.peerb.app.HUMAN
import us.peerb.app.Person
import us.peerb.app.Snapshot

/** Something to write: to an address, or to whoever gets picked; in a thread, answering a message, when replying. */
data class Draft(val to: String? = null, val thread: String = "", val replyTo: String = "")

/** Writing as you (user/human) to anyone you can reach. */
@Composable
fun WriteScreen(draft: Draft, snap: Snapshot?, onDone: () -> Unit, onStatus: (Status) -> Unit) {
    var to by rememberSaveable(draft) { mutableStateOf(draft.to) }
    BackHandler { if (to != null && draft.to == null) to = null else onDone() }
    val target = to
    if (target == null) {
        Pick(snap, onCancel = onDone) { to = it.address }
    } else {
        Compose(target, draft.thread, draft.replyTo, snap?.self.orEmpty(), onCancel = onDone, onSent = { onStatus(it); onDone() }, onError = onStatus)
    }
}

@Composable
private fun Pick(snap: Snapshot?, onCancel: () -> Unit, onPick: (Person) -> Unit) {
    val c = LocalCasing.current
    val people = snap?.people.orEmpty()
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                BackLink(onCancel)
                Spacer(Modifier.weight(1f))
                SectionLabel("write to", Pal.teal)
            }
            Text("as ${snap?.self ?: "you"}/$HUMAN, so replies come back here", style = Type.monoSmall, color = c.muted, modifier = Modifier.padding(top = 8.dp))
        }
        if (people.isEmpty()) {
            Text("Nobody is reachable right now.", color = Pal.red, modifier = Modifier.padding(16.dp))
        }
        LazyColumn(Modifier.weight(1f)) {
            var user = ""
            for (p in people) {
                if (p.user != user) {
                    user = p.user
                    item(key = "u-$user") {
                        Text(
                            if (p.foreign) user.uppercase() else "YOURS",
                            style = Type.label,
                            color = c.muted,
                            modifier = Modifier.padding(start = 16.dp, top = 14.dp, bottom = 4.dp),
                        )
                    }
                }
                item(key = p.address) {
                    Row(
                        Modifier
                            .fillMaxWidth()
                            .clickable { onPick(p) }
                            .padding(horizontal = 16.dp, vertical = 14.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Text(p.name, style = Type.mono, fontWeight = FontWeight.Bold, color = c.fg, modifier = Modifier.widthIn(min = 120.dp).padding(end = 10.dp), maxLines = 1, overflow = TextOverflow.Ellipsis)
                        if (p.human) {
                            Text("the person, to read", style = Type.monoSmall, color = c.muted)
                        } else {
                            Text(p.status, style = Type.monoSmall, color = if (p.status == "idle") Pal.teal else Pal.mustard, modifier = Modifier.widthIn(min = 56.dp).padding(end = 10.dp))
                            val detail = listOf(p.host, p.version, if (p.unverified) "channels unverified" else "").filter { it.isNotEmpty() }.joinToString("  ")
                            Text(detail, style = Type.monoSmall, color = if (p.unverified) Pal.mustard else c.muted, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun Compose(to: String, thread: String, replyTo: String, self: String, onCancel: () -> Unit, onSent: (Status) -> Unit, onError: (Status) -> Unit) {
    val c = LocalCasing.current
    val scope = rememberCoroutineScope()
    var text by rememberSaveable(to, thread) { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.weight(1f).padding(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                BackLink(onCancel)
                Spacer(Modifier.weight(1f))
                SectionLabel(if (thread.isEmpty()) "write" else "reply", Pal.teal)
            }
            Spacer(Modifier.padding(top = 10.dp))
            Field("to", to)
            Field("from", "${self.ifEmpty { "you" }}/$HUMAN", note = if (thread.isEmpty()) null else "in thread $thread")
            Spacer(Modifier.padding(top = 10.dp))
            BasicTextField(
                text,
                { text = it },
                textStyle = Type.screen,
                cursorBrush = SolidColor(Pal.amber),
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 160.dp)
                    .focusRequester(focus)
                    .border(3.dp, Pal.beige, RoundedCornerShape(6.dp))
                    .padding(3.dp)
                    .background(Pal.screen, RoundedCornerShape(3.dp))
                    .padding(14.dp),
                decorationBox = { inner ->
                    Box {
                        if (text.isEmpty()) Text("…", style = Type.screenNote)
                        inner()
                    }
                },
            )
        }
        Stripe(height = 3.dp)
        Row(Modifier.fillMaxWidth().background(c.panel).padding(12.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Keycap("Cancel", onCancel, Modifier.weight(1f), key = "Q")
            Keycap(
                "Send",
                {
                    busy = true
                    scope.launch {
                        try {
                            onSent(Status(Core.send(to, text.trim(), thread, replyTo), false))
                        } catch (e: Exception) {
                            onError(Status(e.message ?: "couldn't send", true))
                        } finally {
                            busy = false
                        }
                    }
                },
                Modifier.weight(2f),
                key = "↵",
                face = Pal.teal,
                enabled = text.isNotBlank() && !busy,
            )
        }
    }
}

/** An address as the lists show it: without the ref, and without your own name. */
fun short(address: String, self: String): String {
    if (address == "$self/" || address == "$self/$HUMAN") return "you"
    return address.substringBefore("#").removePrefix("$self/")
}

/** The keycap that starts writing, for the bottom of a list. */
@Composable
fun WriteKey(onWrite: () -> Unit, modifier: Modifier = Modifier) {
    Keycap("Write", onWrite, modifier.padding(16.dp), key = "✎", face = Pal.teal)
}
