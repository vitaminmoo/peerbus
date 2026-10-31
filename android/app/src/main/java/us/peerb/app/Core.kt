package us.peerb.app

import android.content.Context
import android.os.Build
import android.provider.Settings
import android.util.Log
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import kotlinx.coroutines.withContext
import us.peerb.mobile.Client
import us.peerb.mobile.Listener
import us.peerb.mobile.Mobile
import java.io.File

/**
 * The Go core (../mobile) and what the UI sees of it. The phone is on the
 * mesh only while the app is on screen; in the background, your sessions
 * push it sealed notes through Firebase instead.
 */
object Core {
    /** Whether background notifications can reach this phone. */
    enum class Push { Waiting, Ready, Unconfigured, Failed }

    private lateinit var app: Context
    private lateinit var client: Client
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val lock = Mutex()
    private var held = false
    @Volatile private var token: String? = null

    private val _paired = MutableStateFlow(false)
    val paired: StateFlow<Boolean> = _paired.asStateFlow()

    private val _snapshot = MutableStateFlow<Snapshot?>(null)
    val snapshot: StateFlow<Snapshot?> = _snapshot.asStateFlow()

    /** Why the phone couldn't join the mesh, if it couldn't. */
    private val _problem = MutableStateFlow<String?>(null)
    val problem: StateFlow<String?> = _problem.asStateFlow()

    private val _push = MutableStateFlow(Push.Waiting)
    val push: StateFlow<Push> = _push.asStateFlow()

    val fingerprint: String get() = client.fingerprint()
    val name: String get() = client.name()
    lateinit var machine: String
        private set

    private val listener = object : Listener {
        override fun changed(snapshot: String) {
            val s = Snapshot.parse(snapshot)
            clearHandled(_snapshot.value, s)
            _snapshot.value = s
        }

        override fun held(id: String, title: String, text: String) {
            // With push working, your sessions already notified the phone.
            if (_push.value != Push.Ready) Notifier.held(app, id, title, text)
        }
    }

    /** Clears notifications for messages that left a machine still online: dealt with, not unreachable. */
    private fun clearHandled(old: Snapshot?, new: Snapshot) {
        val online = new.sessions.map { it.host }.toSet()
        val now = new.items.map { it.id }.toSet()
        for (h in old?.items.orEmpty()) {
            if (h.id !in now && h.machine in online) Notifier.cancel(app, h.id)
        }
    }

    fun init(context: Context) {
        app = context.applicationContext
        machine = Settings.Global.getString(app.contentResolver, Settings.Global.DEVICE_NAME) ?: Build.MODEL
        client = Mobile.open(File(app.filesDir, "peerbus").path, machine)
        _paired.value = client.paired()
    }

    /** The app is on screen: join the mesh and refresh quickly. */
    fun hold() = scope.launch {
        lock.withLock {
            held = true
            startLocked()
            client.setForeground(true)
        }
    }

    fun release() = scope.launch {
        lock.withLock {
            held = false
            client.stop()
        }
    }

    private fun startLocked() {
        if (!held || !_paired.value || client.running()) return
        try {
            client.start(listener)
            _problem.value = null
        } catch (e: Exception) {
            Log.w("peerbus", "start", e)
            _problem.value = e.message
        }
    }

    fun pushState(p: Push) {
        _push.value = p
    }

    /** Firebase registration finished; ready once a token has reached the core. */
    fun pushRegistered() = scope.launch {
        if (client.pushToken().isNotEmpty()) _push.value = Push.Ready
    }

    /** This phone's FCM token; sessions learn it the next time the phone reaches them. */
    fun setPushToken(token: String) = scope.launch {
        this@Core.token = token
        try {
            client.setPushToken(token)
            _push.value = Push.Ready
        } catch (e: Exception) {
            Log.w("peerbus", "push token", e)
            _push.value = Push.Failed
        }
    }

    /** Opens a sealed note from a push. */
    fun unseal(sealed: String): String = client.unseal(sealed)

    suspend fun pair(code: String) = withContext(Dispatchers.IO) {
        lock.withLock {
            client.pair(code.trim())
            token?.let { client.setPushToken(it) } // forgetting an earlier pairing cleared it
            _paired.value = true
            _snapshot.value = null
            startLocked()
            client.setForeground(true)
        }
    }

    suspend fun forget() = withContext(Dispatchers.IO) {
        lock.withLock {
            client.forget()
            _paired.value = false
            _snapshot.value = null
        }
    }

    suspend fun approve(id: String, toSid: String = ""): String = withContext(Dispatchers.IO) { client.approve(id, toSid) }

    suspend fun deny(id: String): String = withContext(Dispatchers.IO) { client.deny(id) }

    /** Writes as you (user/human); replies come back to read. */
    suspend fun send(to: String, text: String, thread: String = ""): String = withContext(Dispatchers.IO) { client.send(to, text, thread) }

    suspend fun history(): List<Record> = withContext(Dispatchers.IO) { Record.parseAll(client.history()) }

    suspend fun fullText(key: String): String = withContext(Dispatchers.IO) { client.fullText(key) }
}
