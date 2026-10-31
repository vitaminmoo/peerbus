package us.peerb.app.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import kotlinx.coroutines.launch
import us.peerb.app.Core

/** This phone: who it is, whether pushes can reach it, and unpairing. */
@Composable
fun UnitPage() {
    val c = LocalCasing.current
    val ctx = LocalContext.current
    val scope = rememberCoroutineScope()
    val push by Core.push.collectAsStateWithLifecycle()
    var forgetting by remember { mutableStateOf(false) }
    // Bumped on resume so the permission is read afresh after Settings.
    var resumed by remember { mutableIntStateOf(0) }
    LifecycleResumeEffect(Unit) {
        resumed++
        onPauseOrDispose { }
    }
    val notifyAsk = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { resumed++ }
    val canNotify = resumed >= 0 && (Build.VERSION.SDK_INT < 33 ||
        ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED)

    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp)) {
        SectionLabel("identity", Pal.mustard)
        Spacer(Modifier.height(10.dp))
        Field("name", Core.name.ifEmpty { "(not set; peerbus id -name)" })
        Field("key", Core.fingerprint, note = "matches `peerbus id` on your computers")
        Field("unit", Core.machine)

        Spacer(Modifier.height(22.dp))
        SectionLabel("alerts", Pal.orange)
        Spacer(Modifier.height(10.dp))
        val (state, detail) = when (push) {
            Core.Push.Ready -> "PUSH READY" to "Your sessions push sealed notes when a message is held, through the rendezvous server and Firebase. Each machine learns this phone while the app is open with a session running there."
            Core.Push.Waiting -> "PUSH STARTING" to "Getting a push token from Firebase."
            Core.Push.Unconfigured -> "NO PUSH" to "This build has no Firebase project (android/firebase.properties), so it only notifies while open."
            Core.Push.Failed -> "PUSH FAILED" to "Firebase didn't give this phone a push token; it only notifies while open."
        }
        PhosphorScreen(Modifier.fillMaxWidth()) {
            Text("> $state", style = Type.screen.copy(color = if (push == Core.Push.Ready) Pal.amber else Pal.red))
            Text(detail, style = Type.screenNote, modifier = Modifier.padding(top = 4.dp))
        }
        if (!canNotify) {
            Spacer(Modifier.height(10.dp))
            Keycap("Allow notifications", { notifyAsk.launch(Manifest.permission.POST_NOTIFICATIONS) }, Modifier.fillMaxWidth(), key = "!", face = Pal.mustard)
        }

        Spacer(Modifier.height(28.dp))
        SectionLabel("danger", Pal.red)
        Spacer(Modifier.height(10.dp))
        Text("Forgetting removes your identity key and peers from this phone, and tells the sessions it can reach to stop pushing to it. Your computers keep theirs.", style = Type.bodySmall, color = c.muted)
        Spacer(Modifier.height(10.dp))
        Keycap("Forget this phone", { forgetting = true }, Modifier.fillMaxWidth(), key = "✕", face = Pal.red)
    }

    if (forgetting) {
        Dialog(onDismissRequest = { forgetting = false }) {
            Column(Modifier.background(c.panel, RoundedCornerShape(6.dp)).border(2.dp, Pal.ink, RoundedCornerShape(6.dp)).padding(18.dp)) {
                SectionLabel("forget", Pal.red)
                Text("Forget this phone?", style = Type.title, color = c.fg, modifier = Modifier.padding(top = 14.dp))
                Text("You'd pair it again with `peerbus phone`.", color = c.muted, modifier = Modifier.padding(top = 4.dp, bottom = 18.dp))
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    Keycap("No", { forgetting = false }, Modifier.weight(1f), key = "N")
                    Keycap("Yes", {
                        forgetting = false
                        scope.launch { Core.forget() }
                    }, Modifier.weight(1f), key = "Y", face = Pal.red)
                }
            }
        }
    }
}
