package us.peerb.app.ui

import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.RepeatMode
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.interaction.MutableInteractionSource
import androidx.compose.foundation.interaction.collectIsPressedAsState
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxScope
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.Typography
import androidx.compose.material3.minimumInteractiveComponentSize
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.Immutable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.draw.drawWithContent
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.Shadow
import androidx.compose.ui.graphics.drawscope.clipRect
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

// Cassette futurism, as on peerb.us and in `peerbus tui`: beige casings,
// stencilled plates, a four-colour stripe, hazard-striped section labels,
// amber phosphor screens.
object Pal {
    val beige = Color(0xFFD6CEB6)
    val beigeLo = Color(0xFFA99F84)
    val ink = Color(0xFF2B2920)
    val screen = Color(0xFF14130E)
    val amber = Color(0xFFF3B44A)
    val amberLo = Color(0xFFA4803E)
    val orange = Color(0xFFE2772B)
    val teal = Color(0xFF3F9A8E)
    val red = Color(0xFFC9443A)
    val mustard = Color(0xFFD9A521)
    val glitch = Color(0xFF7FE9D2)

    /** Section colours, as the TUI's lanes, in Section order. */
    val lanes = listOf(glitch, orange, mustard, teal)
}

@Immutable
data class Casing(val bg: Color, val panel: Color, val fg: Color, val muted: Color, val rule: Color, val dark: Boolean)

private val lightCasing = Casing(Color(0xFFE8E2D1), Color(0xFFF3EEE1), Color(0xFF2B2920), Color(0xFF6B6454), Color(0xFFC8BFA6), false)
private val darkCasing = Casing(Color(0xFF191813), Color(0xFF22201A), Color(0xFFE6DFCA), Color(0xFFA39C87), Color(0xFF3B372C), true)

val LocalCasing = staticCompositionLocalOf { darkCasing }

val Mono = FontFamily.Monospace

@Composable
fun PeerbusTheme(content: @Composable () -> Unit) {
    val c = if (isSystemInDarkTheme()) darkCasing else lightCasing
    val scheme = if (c.dark) {
        darkColorScheme(primary = Pal.orange, background = c.bg, surface = c.panel, onBackground = c.fg, onSurface = c.fg)
    } else {
        lightColorScheme(primary = Pal.orange, background = c.bg, surface = c.panel, onBackground = c.fg, onSurface = c.fg)
    }
    val type = Typography(bodyLarge = Type.body, bodyMedium = Type.bodySmall, labelLarge = Type.label, titleLarge = Type.title)
    CompositionLocalProvider(LocalCasing provides c) {
        MaterialTheme(colorScheme = scheme, typography = type, content = content)
    }
}

/** The casing: background with the site's faint ruled lines. */
fun Modifier.ruled(c: Casing) = drawBehind {
    drawRect(c.bg)
    val step = 26.dp.toPx()
    val line = Color(0x12807050)
    var y = step
    while (y < size.height) {
        drawLine(line, Offset(0f, y), Offset(size.width, y), 1f)
        y += step
    }
}

/** Mustard, orange, red, teal. */
@Composable
fun Stripe(modifier: Modifier = Modifier, height: Dp = 4.dp) {
    Row(modifier.fillMaxWidth().height(height)) {
        for (c in listOf(Pal.mustard, Pal.orange, Pal.red, Pal.teal)) {
            Box(Modifier.weight(1f).height(height).background(c))
        }
    }
}

/** Diagonal hazard stripes in colour over ink, or over ground. */
@Composable
fun Hazard(color: Color, modifier: Modifier = Modifier, band: Dp = 5.dp, ground: Color = Pal.ink) {
    Canvas(modifier.clipToBounds()) {
        drawRect(ground)
        val w = band.toPx()
        var x = -size.height
        while (x < size.width + size.height) {
            val p = Path().apply {
                moveTo(x, size.height)
                lineTo(x + w, size.height)
                lineTo(x + w + size.height, 0f)
                lineTo(x + size.height, 0f)
                close()
            }
            drawPath(p, color)
            x += 2 * w
        }
    }
}

/**
 * The type scale, on Material 3's sizes: body 16sp, labels 14sp, titles
 * 22sp. Text is in sp so it follows the user's font size setting; every
 * screen takes its text from here rather than setting sizes itself.
 */
object Type {
    private val glow = Shadow(Pal.amber.copy(alpha = 0.55f), blurRadius = 10f)

    /** The masthead on the pairing screen (displaySmall). */
    val display = TextStyle(fontFamily = Mono, fontWeight = FontWeight.ExtraBold, fontSize = 36.sp, lineHeight = 44.sp, letterSpacing = 2.sp)
    /** PEERBUS in the header (titleLarge). */
    val brand = TextStyle(fontFamily = Mono, fontWeight = FontWeight.ExtraBold, fontSize = 22.sp, letterSpacing = 3.sp)
    /** Dialog titles (titleLarge). */
    val title = TextStyle(fontWeight = FontWeight.Bold, fontSize = 22.sp, lineHeight = 28.sp)
    /** Stencilled labels: sections, tabs, field names (labelLarge). */
    val label = TextStyle(fontFamily = Mono, fontWeight = FontWeight.Bold, fontSize = 14.sp, lineHeight = 20.sp, letterSpacing = 1.4.sp)
    /** Model plates (labelMedium). */
    val plate = TextStyle(fontFamily = Mono, fontWeight = FontWeight.Bold, fontSize = 12.sp, letterSpacing = 2.sp)
    /** Key legends (titleMedium). */
    val key = TextStyle(fontFamily = Mono, fontWeight = FontWeight.Bold, fontSize = 16.sp, letterSpacing = 1.4.sp)
    /** Prose (bodyLarge, bodyMedium). */
    val body = TextStyle(fontSize = 16.sp, lineHeight = 24.sp)
    val bodySmall = TextStyle(fontSize = 14.sp, lineHeight = 20.sp)
    /** Addresses, names and other values. */
    val mono = TextStyle(fontFamily = Mono, fontSize = 16.sp, lineHeight = 24.sp)
    val monoSmall = TextStyle(fontFamily = Mono, fontSize = 14.sp, lineHeight = 20.sp)
    /** Amber phosphor: message text and screen readouts. */
    val screen = TextStyle(fontFamily = Mono, fontSize = 18.sp, lineHeight = 26.sp, color = Pal.amber, shadow = glow)
    val screenLarge = screen.copy(fontSize = 22.sp, lineHeight = 30.sp)
    /** Dimmer notes on a phosphor screen. */
    val screenNote = TextStyle(fontFamily = Mono, fontSize = 15.sp, lineHeight = 22.sp, color = Pal.amberLo)
}

/** A hazard-striped section tab, as the TUI's ▚▚ labels. */
@Composable
fun SectionLabel(text: String, color: Color, count: Int? = null, modifier: Modifier = Modifier) {
    val c = LocalCasing.current
    Row(modifier, verticalAlignment = Alignment.CenterVertically) {
        Hazard(color, Modifier.size(26.dp, 16.dp))
        Text(text.uppercase(), style = Type.label, color = c.fg, modifier = Modifier.padding(start = 8.dp))
        if (count != null) Text(" $count", style = Type.label, color = color)
    }
}

/** A stencilled model plate. */
@Composable
fun Plate(text: String, modifier: Modifier = Modifier, bg: Color = Pal.beige, fg: Color = Pal.ink) {
    Text(
        text.uppercase(),
        style = Type.plate,
        color = fg,
        modifier = modifier
            .background(bg, RoundedCornerShape(3.dp))
            .border(1.5.dp, Pal.ink, RoundedCornerShape(3.dp))
            .padding(horizontal = 8.dp, vertical = 3.dp),
    )
}

/** A chunky deck key that travels when pressed. */
@Composable
fun Keycap(
    label: String,
    onClick: () -> Unit,
    modifier: Modifier = Modifier,
    key: String? = null,
    face: Color = Pal.beige,
    enabled: Boolean = true,
) {
    val interaction = remember { MutableInteractionSource() }
    val pressed by interaction.collectIsPressedAsState()
    val travel = 4.dp
    val drop = if (pressed && enabled) travel else 0.dp
    val shape = RoundedCornerShape(4.dp)
    val faceColor = if (enabled) face else face.copy(alpha = 0.45f)
    Box(
        modifier
            .clickable(interaction, indication = null, enabled = enabled, onClick = onClick)
            .padding(top = drop, bottom = travel - drop)
            .drawBehind {
                val r = CornerRadius(4.dp.toPx())
                drawRoundRect(Pal.ink, topLeft = Offset(0f, (travel - drop).toPx()), size = size, cornerRadius = r)
            }
            .background(faceColor, shape)
            .border(1.5.dp, Pal.ink, shape)
            .heightIn(min = 56.dp)
            .padding(horizontal = 18.dp, vertical = 14.dp),
        contentAlignment = Alignment.Center,
    ) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.Center) {
            if (key != null) {
                Text(
                    key,
                    style = Type.label,
                    color = faceColor,
                    modifier = Modifier.background(Pal.ink, RoundedCornerShape(2.dp)).padding(horizontal = 6.dp, vertical = 2.dp),
                )
                Box(Modifier.width(10.dp))
            }
            Text(label.uppercase(), style = Type.key, color = Pal.ink)
        }
    }
}

/** An amber phosphor screen in a beige bezel, with scanlines. */
@Composable
fun PhosphorScreen(modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    Box(
        modifier
            .border(3.dp, Pal.beige, RoundedCornerShape(6.dp))
            .padding(3.dp)
            .background(Pal.screen, RoundedCornerShape(3.dp))
            .drawWithContent {
                drawContent()
                clipRect {
                    val step = 3.dp.toPx()
                    var y = 0f
                    while (y < size.height) {
                        drawRect(Color(0x40000000), Offset(0f, y + step * 0.66f), androidx.compose.ui.geometry.Size(size.width, step * 0.34f))
                        y += step
                    }
                }
            }
            .padding(horizontal = 16.dp, vertical = 14.dp),
    ) {
        Column(content = content)
    }
}

/** A blinking block cursor for phosphor text. */
@Composable
fun Cursor(modifier: Modifier = Modifier) {
    val t = rememberInfiniteTransition(label = "cursor")
    val a by t.animateFloat(1f, 0f, infiniteRepeatable(tween(530, easing = { if (it < 0.5f) 0f else 1f }), RepeatMode.Restart), label = "blink")
    Text("█", style = Type.screen, modifier = modifier, color = Pal.amber.copy(alpha = a))
}

/** A dot that pulses while waiting. */
@Composable
fun Pulse(color: Color, on: Boolean, modifier: Modifier = Modifier) {
    val t = rememberInfiniteTransition(label = "pulse")
    val a by t.animateFloat(1f, 0.25f, infiniteRepeatable(tween(700, easing = LinearEasing), RepeatMode.Reverse), label = "a")
    Text(if (on) "●" else "○", color = color.copy(alpha = if (on) 1f else a), style = Type.label, modifier = modifier)
}

/** A labelled field, as the TUI's FROM / TO / HELD lines. */
@Composable
fun Field(label: String, value: String, modifier: Modifier = Modifier, note: String? = null) {
    val c = LocalCasing.current
    Row(modifier.padding(vertical = 4.dp)) {
        // Grows rather than clips when the user's font size is large.
        Text(label.uppercase(), style = Type.label, color = c.muted, modifier = Modifier.widthIn(min = 76.dp).padding(top = 2.dp, end = 8.dp))
        Column {
            Text(value, style = Type.mono, color = c.fg)
            if (note != null) Text(note, style = Type.monoSmall, color = c.muted)
        }
    }
}

/** A plain casing panel with a coloured edge. */
@Composable
fun Panel(edge: Color, modifier: Modifier = Modifier, content: @Composable BoxScope.() -> Unit) {
    val c = LocalCasing.current
    Box(
        modifier
            .background(c.panel, RoundedCornerShape(4.dp))
            .border(1.dp, c.rule, RoundedCornerShape(4.dp))
            .drawBehind { drawRect(edge, size = androidx.compose.ui.geometry.Size(4.dp.toPx(), size.height)) }
            .padding(start = 18.dp, end = 14.dp, top = 14.dp, bottom = 14.dp),
        content = content,
    )
}

/** "◂ BACK", with a full-size touch target. */
@Composable
fun BackLink(onClick: () -> Unit) {
    Box(
        Modifier.minimumInteractiveComponentSize().clickable(onClick = onClick).padding(end = 12.dp),
        contentAlignment = Alignment.CenterStart,
    ) {
        Text("◂ BACK", style = Type.label, color = LocalCasing.current.muted)
    }
}
