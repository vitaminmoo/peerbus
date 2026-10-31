package us.peerb.app

import android.Manifest
import android.content.Intent
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.SystemBarStyle
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.WindowInsetsSides
import androidx.compose.foundation.layout.only
import androidx.compose.foundation.layout.plus
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.layout.windowInsetsPadding
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import us.peerb.app.ui.Draft
import us.peerb.app.ui.Header
import us.peerb.app.ui.HistoryPage
import us.peerb.app.ui.WriteScreen
import us.peerb.app.ui.InboxPage
import us.peerb.app.ui.LocalCasing
import us.peerb.app.ui.PairScreen
import us.peerb.app.ui.PeerbusTheme
import us.peerb.app.ui.Status
import us.peerb.app.ui.StatusLine
import us.peerb.app.ui.Tab
import us.peerb.app.ui.UnitPage
import us.peerb.app.ui.ruled

class MainActivity : ComponentActivity() {
    /** A held message to open, from a notification. */
    private val opening = mutableStateOf<String?>(null)

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // The header is always a beige plate, so dark status bar icons in either theme.
        enableEdgeToEdge(statusBarStyle = SystemBarStyle.light(android.graphics.Color.TRANSPARENT, android.graphics.Color.TRANSPARENT))
        opening.value = intent.getStringExtra(Notifier.EXTRA_HELD)
        setContent { PeerbusTheme { App(opening.value) { opening.value = null } } }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        intent.getStringExtra(Notifier.EXTRA_HELD)?.let { opening.value = it }
    }
}

@Composable
private fun App(fromNotification: String?, consumed: () -> Unit) {
    val paired by Core.paired.collectAsStateWithLifecycle()
    val c = LocalCasing.current
    Box(Modifier.fillMaxSize().ruled(c).windowInsetsPadding(WindowInsets.safeDrawing.only(WindowInsetsSides.Horizontal + WindowInsetsSides.Bottom)), contentAlignment = Alignment.TopCenter) {
        // Phone-width columns on tablets and unfolded foldables.
        Box(Modifier.widthIn(max = 640.dp).fillMaxSize()) {
            if (!paired) PairScreen() else Main(fromNotification, consumed)
        }
    }
}

@Composable
private fun Main(fromNotification: String?, consumed: () -> Unit) {
    val snap by Core.snapshot.collectAsStateWithLifecycle()
    val problem by Core.problem.collectAsStateWithLifecycle()
    var tab by rememberSaveable { mutableStateOf(Tab.Inbox) }
    var open by rememberSaveable { mutableStateOf<String?>(null) }
    var status by androidx.compose.runtime.remember { mutableStateOf<Status?>(null) }
    var writing by androidx.compose.runtime.remember { mutableStateOf<Draft?>(null) }

    val notifyAsk = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { }
    LaunchedEffect(Unit) {
        if (Build.VERSION.SDK_INT >= 33) notifyAsk.launch(Manifest.permission.POST_NOTIFICATIONS)
    }
    LaunchedEffect(fromNotification) {
        if (fromNotification != null) {
            tab = Tab.Inbox
            open = fromNotification
            consumed()
        }
    }

    Column(Modifier.fillMaxSize()) {
        Header(tab, { tab = it; open = null; writing = null }, snap, problem)
        Box(Modifier.weight(1f)) {
            val draft = writing
            when {
                draft != null -> WriteScreen(draft, snap, onDone = { writing = null }, onStatus = { status = it })
                tab == Tab.Inbox -> InboxPage(snap, open, { open = it }, { status = it }, { writing = it })
                tab == Tab.History -> HistoryPage(snap?.self.orEmpty(), onWrite = { writing = it })
                else -> UnitPage()
            }
        }
        StatusLine(status)
    }
}
