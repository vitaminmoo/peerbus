package us.peerb.app

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import androidx.core.app.NotificationCompat
import androidx.core.content.ContextCompat

/** Held messages as notifications, one per message, gone once it's dealt with anywhere. */
object Notifier {
    private const val HELD_CHANNEL = "held"
    private const val HELD_ID = 1
    const val EXTRA_HELD = "us.peerb.app.HELD"

    private val amber = 0xFFF3B44A.toInt()

    fun channels(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(HELD_CHANNEL, ctx.getString(R.string.channel_held), NotificationManager.IMPORTANCE_HIGH).apply {
                description = ctx.getString(R.string.channel_held_desc)
                enableLights(true)
                lightColor = amber
            },
        )
    }

    private fun open(ctx: Context, id: String?): PendingIntent {
        val intent = Intent(ctx, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP or Intent.FLAG_ACTIVITY_CLEAR_TOP)
        if (id != null) intent.putExtra(EXTRA_HELD, id)
        return PendingIntent.getActivity(ctx, id?.hashCode() ?: 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }

    fun held(ctx: Context, id: String, title: String, text: String) {
        if (ContextCompat.checkSelfPermission(ctx, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) return
        val n = NotificationCompat.Builder(ctx, HELD_CHANNEL)
            .setSmallIcon(R.drawable.ic_stat)
            .setColor(amber)
            .setContentTitle(title)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setCategory(NotificationCompat.CATEGORY_MESSAGE)
            .setPriority(NotificationCompat.PRIORITY_HIGH)
            .setContentIntent(open(ctx, id))
            .setAutoCancel(true)
            .setOnlyAlertOnce(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(id, HELD_ID, n)
    }

    /** Clears a message's notification once it's been dealt with, here or anywhere. */
    fun cancel(ctx: Context, id: String) {
        ctx.getSystemService(NotificationManager::class.java).cancel(id, HELD_ID)
    }
}
