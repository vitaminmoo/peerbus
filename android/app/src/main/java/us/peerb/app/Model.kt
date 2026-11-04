package us.peerb.app

import org.json.JSONObject

/** Where a held message sits, as in `peerbus tui` (internal/held's order). */
enum class Section(val title: String) {
    ToRead("to you · read"),
    ForSession("held for a session"),
    ForYou("for you · pick a session"),
    Yours("yours · waiting to send"),
}

/** One held message, from the Go core's snapshot (mobile.itemJSON). */
data class Held(
    val id: String,
    /** The message's own id, which replies name. */
    val msgId: String,
    val from: String,
    val to: String,
    val thread: String,
    val peer: String,
    val target: String,
    val heldAt: Long,
    val text: String,
    val section: Section,
    val machine: String,
    val targetLabel: String,
    /** Made by `peerbus fake`: approving it delivers nothing. */
    val fake: Boolean,
    /** To you (user/human), to read: reply or dismiss. */
    val note: Boolean,
)

/** Someone you can write to: a session, yours or a friend's, or a friend as a person. */
data class Person(
    val address: String,
    val user: String,
    val name: String,
    val host: String,
    val status: String,
    val foreign: Boolean,
    val channels: String,
    /** The peerbus it runs; empty when an older one didn't say. */
    val version: String,
) {
    val human get() = name == HUMAN
    /** Connected, but its agent may not see what you send. */
    val unverified get() = !human && channels != "on"
}

/** The session name that is a person rather than an agent (proto.Human). */
const val HUMAN = "human"

/** One message from the history, merged from every machine that logged it. */
data class Record(
    val key: String,
    val id: String,
    val thread: String,
    val from: String,
    val to: String,
    /** Your name for the other person; empty between your own sessions. */
    val peer: String,
    val status: String,
    val at: Long,
    val text: String,
    val cut: Boolean,
    val fake: Boolean,
    val machines: List<String>,
    /** The id of the message this answers, if any. */
    val replyTo: String,
) {
    /** Which conversation it belongs to; one logged without a thread is its own. */
    val threadKey get() = thread.ifEmpty { key }

    companion object {
        fun parseAll(json: String): List<Record> {
            val a = org.json.JSONArray(json)
            return List(a.length()) { i ->
                val r = a.getJSONObject(i)
                val ms = r.optJSONArray("machines")
                Record(
                    key = r.getString("key"),
                    id = r.optString("id"),
                    thread = r.optString("thread"),
                    from = r.optString("from"),
                    to = r.optString("to"),
                    peer = r.optString("peer"),
                    status = r.optString("status"),
                    at = r.optLong("at"),
                    text = r.optString("text"),
                    cut = r.optBoolean("cut"),
                    fake = r.optBoolean("fake"),
                    machines = List(ms?.length() ?: 0) { ms!!.getString(it) },
                    replyTo = r.optString("replyTo"),
                )
            }
        }
    }
}

/** One of your sessions, on any machine. */
data class Sess(
    val sid: String,
    val name: String,
    val host: String,
    val status: String,
    val cwd: String,
    val addr: String,
    val channels: String,
) {
    val label get() = "$name@$host"
}

data class Snapshot(
    val live: Boolean,
    val self: String,
    val machines: Int,
    val items: List<Held>,
    val sessions: List<Sess>,
    val people: List<Person>,
) {
    fun find(id: String) = items.firstOrNull { it.id == id }

    companion object {
        fun parse(json: String): Snapshot {
            val o = JSONObject(json)
            val items = o.getJSONArray("items")
            val sessions = o.getJSONArray("sessions")
            val people = o.optJSONArray("people") ?: org.json.JSONArray()
            return Snapshot(
                live = o.optBoolean("live"),
                self = o.optString("self"),
                machines = o.optInt("machines"),
                items = List(items.length()) { i ->
                    val h = items.getJSONObject(i)
                    Held(
                        id = h.getString("id"),
                        msgId = h.optString("msgId"),
                        from = h.optString("from"),
                        to = h.optString("to"),
                        thread = h.optString("thread"),
                        peer = h.optString("peer"),
                        target = h.optString("target"),
                        heldAt = h.optLong("heldAt"),
                        text = h.optString("text"),
                        section = Section.entries.getOrElse(h.optInt("section")) { Section.ForSession },
                        machine = h.optString("machine"),
                        targetLabel = h.optString("targetLabel"),
                        fake = h.optBoolean("fake"),
                        note = h.optBoolean("note"),
                    )
                },
                sessions = List(sessions.length()) { i ->
                    val s = sessions.getJSONObject(i)
                    Sess(
                        sid = s.getString("sid"),
                        name = s.optString("name"),
                        host = s.optString("host"),
                        status = s.optString("status"),
                        cwd = s.optString("cwd"),
                        addr = s.optString("addr"),
                        channels = s.optString("channels"),
                    )
                },
                people = List(people.length()) { i ->
                    val p = people.getJSONObject(i)
                    Person(
                        address = p.getString("address"),
                        user = p.optString("user"),
                        name = p.optString("name"),
                        host = p.optString("host"),
                        status = p.optString("status"),
                        foreign = p.optBoolean("foreign"),
                        channels = p.optString("channels"),
                        version = p.optString("version"),
                    )
                },
            )
        }
    }
}

/** How long ago, as the TUI says it. */
fun ago(unix: Long, now: Long = System.currentTimeMillis() / 1000): String {
    val d = now - unix
    return when {
        d < 60 -> "now"
        d < 3600 -> "${d / 60}m"
        d < 48 * 3600 -> "${d / 3600}h"
        else -> "${d / 86400}d"
    }
}

fun plural(n: Int, word: String) = if (n == 1) "1 $word" else "$n ${word}s"
