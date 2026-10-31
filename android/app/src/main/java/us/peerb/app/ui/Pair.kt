package us.peerb.app.ui

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.compose.CameraXViewfinder
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.Preview
import androidx.camera.core.SurfaceRequest
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.lifecycle.awaitInstance
import androidx.camera.mlkit.vision.MlKitAnalyzer
import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LocalLifecycleOwner
import com.google.mlkit.vision.barcode.BarcodeScannerOptions
import com.google.mlkit.vision.barcode.BarcodeScanning
import com.google.mlkit.vision.barcode.common.Barcode
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.launch
import us.peerb.app.Core

private enum class PairMode { Idle, Scanning, Pasting, Pairing }

/** Not paired yet: `peerbus phone` on a computer shows the code to scan. */
@Composable
fun PairScreen() {
    val c = LocalCasing.current
    val ctx = LocalContext.current
    val scope = rememberCoroutineScope()
    var mode by remember { mutableStateOf(PairMode.Idle) }
    var error by remember { mutableStateOf<String?>(null) }
    var pasted by remember { mutableStateOf("") }

    fun pair(code: String) {
        mode = PairMode.Pairing
        error = null
        scope.launch {
            try {
                Core.pair(code)
            } catch (e: Exception) {
                error = e.message ?: "pairing failed"
                mode = PairMode.Idle
            }
        }
    }

    val camera = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { ok ->
        if (ok) mode = PairMode.Scanning else error = "The camera is needed to scan; paste the code instead."
    }

    if (mode == PairMode.Scanning) {
        BackHandler { mode = PairMode.Idle }
        Scanner(onCode = ::pair, onCancel = { mode = PairMode.Idle })
        return
    }

    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState())) {
        Column(Modifier.fillMaxWidth().background(Pal.beige).statusBarsPadding().padding(start = 18.dp, end = 18.dp, top = 22.dp, bottom = 16.dp)) {
            Plate("PB-1 · pocket unit", bg = Color.Transparent)
            Text(
                "PEERBUS",
                style = Type.display,
                color = Pal.ink,
                modifier = Modifier.padding(top = 10.dp),
            )
            Text("Held messages from every one of your machines, in your pocket.", style = Type.body, color = Pal.ink.copy(alpha = 0.75f))
        }
        Stripe(height = 6.dp)
        Column(Modifier.padding(18.dp)) {
            SectionLabel("pair this phone", Pal.orange)
            Spacer(Modifier.height(12.dp))
            PhosphorScreen(Modifier.fillMaxWidth()) {
                if (mode == PairMode.Pairing) {
                    Text("> PAIRING", style = Type.screen)
                    Text("finding your computer through the rendezvous server", style = Type.screenNote)
                    Row(Modifier.padding(top = 4.dp)) { Text("> ", style = Type.screen); Cursor() }
                } else {
                    Text("On your computer, run:", style = Type.screenNote)
                    Text("$ peerbus phone", style = Type.screenLarge, modifier = Modifier.padding(vertical = 6.dp))
                    Text("then scan the code it shows.", style = Type.screenNote)
                }
            }
            Text(
                "The phone becomes one of your devices: it gets your identity key and peer list, as `peerbus invite -self` gives a new machine.",
                style = Type.bodySmall,
                color = c.muted,
                modifier = Modifier.padding(top = 12.dp),
            )
            error?.let {
                Text(it, color = Pal.red, style = Type.monoSmall, modifier = Modifier.padding(top = 12.dp))
            }
            Spacer(Modifier.height(18.dp))
            if (mode != PairMode.Pairing) {
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    Keycap(
                        "Scan code",
                        {
                            if (ContextCompat.checkSelfPermission(ctx, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) {
                                mode = PairMode.Scanning
                            } else {
                                camera.launch(Manifest.permission.CAMERA)
                            }
                        },
                        Modifier.weight(1f),
                        key = "▣",
                        face = Pal.orange,
                    )
                    Keycap("Paste code", { mode = PairMode.Pasting }, Modifier.weight(1f), key = "⎘")
                }
            }
            if (mode == PairMode.Pasting) {
                Spacer(Modifier.height(16.dp))
                BasicTextField(
                    pasted,
                    { pasted = it },
                    textStyle = Type.screen,
                    cursorBrush = SolidColor(Pal.amber),
                    modifier = Modifier
                        .fillMaxWidth()
                        .border(3.dp, Pal.beige, RoundedCornerShape(6.dp))
                        .padding(3.dp)
                        .background(Pal.screen, RoundedCornerShape(3.dp))
                        .padding(12.dp),
                    decorationBox = { inner ->
                        if (pasted.isEmpty()) Text("pb1-…", style = Type.screenNote)
                        inner()
                    },
                )
                Spacer(Modifier.height(12.dp))
                Keycap("Pair", { pair(pasted) }, Modifier.fillMaxWidth(), key = "↵", face = Pal.teal, enabled = pasted.isNotBlank())
            }
        }
    }
}

/** A camera viewfinder in a bezel, scanning for a peerbus invite. */
@Composable
private fun Scanner(onCode: (String) -> Unit, onCancel: () -> Unit) {
    val ctx = LocalContext.current
    val owner = LocalLifecycleOwner.current
    val requests = remember { MutableStateFlow<SurfaceRequest?>(null) }
    val request by requests.collectAsState()
    var done by remember { mutableStateOf(false) }

    LaunchedEffect(owner) {
        val provider = ProcessCameraProvider.awaitInstance(ctx)
        val main = ContextCompat.getMainExecutor(ctx)
        val preview = Preview.Builder().build().apply { setSurfaceProvider { requests.value = it } }
        val scanner = BarcodeScanning.getClient(BarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build())
        val analysis = ImageAnalysis.Builder().setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST).build()
        analysis.setAnalyzer(main, MlKitAnalyzer(listOf(scanner), ImageAnalysis.COORDINATE_SYSTEM_ORIGINAL, main) { result ->
            val code = result.getValue(scanner)?.firstNotNullOfOrNull { b -> b.rawValue?.takeIf { it.startsWith("pb1-", ignoreCase = true) } }
            if (code != null && !done) {
                done = true
                onCode(code)
            }
        })
        provider.unbindAll()
        provider.bindToLifecycle(owner, CameraSelector.DEFAULT_BACK_CAMERA, preview, analysis)
        try {
            awaitCancellation()
        } finally {
            provider.unbindAll()
            scanner.close()
        }
    }

    Column(Modifier.fillMaxSize().background(Pal.screen)) {
        Column(Modifier.fillMaxWidth().background(Pal.beige).statusBarsPadding().padding(horizontal = 16.dp, vertical = 12.dp)) {
            SectionLabel("scan invite", Pal.orange)
        }
        Stripe()
        Box(Modifier.weight(1f).fillMaxWidth().padding(20.dp), contentAlignment = Alignment.Center) {
            Box(
                Modifier
                    .fillMaxWidth()
                    .aspectRatio(1f)
                    .border(4.dp, Pal.beige, RoundedCornerShape(8.dp))
                    .padding(4.dp),
            ) {
                request?.let { CameraXViewfinder(surfaceRequest = it, modifier = Modifier.fillMaxSize()) }
                Reticle(Modifier.fillMaxSize())
            }
        }
        Text(
            "Point at the code from `peerbus phone`.",
            style = Type.screen,
            modifier = Modifier.padding(horizontal = 20.dp),
        )
        Row(Modifier.fillMaxWidth().padding(16.dp)) {
            Keycap("Cancel", onCancel, Modifier.fillMaxWidth(), key = "Q")
        }
    }
}

/** Amber corner brackets and a sweeping scanline. */
@Composable
private fun Reticle(modifier: Modifier) {
    val t = rememberInfiniteTransition(label = "sweep")
    val y by t.animateFloat(0.08f, 0.92f, infiniteRepeatable(tween(1800, easing = LinearEasing), RepeatMode.Reverse), label = "y")
    Canvas(modifier) {
        val inset = size.minDimension * 0.08f
        val arm = size.minDimension * 0.14f
        val w = 4.dp.toPx()
        val l = inset
        val r = size.width - inset
        val top = inset
        val b = size.height - inset
        for ((x, dx) in listOf(l to 1f, r to -1f)) {
            for ((yy, dy) in listOf(top to 1f, b to -1f)) {
                drawLine(Pal.amber, Offset(x, yy), Offset(x + dx * arm, yy), w, StrokeCap.Square)
                drawLine(Pal.amber, Offset(x, yy), Offset(x, yy + dy * arm), w, StrokeCap.Square)
            }
        }
        val sy = size.height * y
        drawRect(
            Brush.verticalGradient(listOf(Color.Transparent, Pal.amber.copy(alpha = 0.35f), Color.Transparent), startY = sy - 24f, endY = sy + 24f),
            topLeft = Offset(l, sy - 24f),
            size = androidx.compose.ui.geometry.Size(r - l, 48f),
        )
        drawLine(Pal.amber, Offset(l, sy), Offset(r, sy), 2f)
    }
}
