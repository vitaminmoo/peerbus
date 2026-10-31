package us.peerb.app

import android.util.Log
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import org.json.JSONObject

/**
 * A sealed note from one of your sessions, relayed by the rendezvous
 * server through Firebase: a message held (show it) or dealt with (clear
 * it). Only this phone can open it; the phone needn't join the mesh.
 */
class PushService : FirebaseMessagingService() {
    override fun onMessageReceived(message: RemoteMessage) {
        val sealed = message.data["p"] ?: return
        val note = try {
            JSONObject(Core.unseal(sealed))
        } catch (e: Exception) {
            Log.w("peerbus", "push note", e)
            return
        }
        val id = note.optString("id")
        when (note.optString("k")) {
            "held" -> Notifier.held(this, id, note.optString("title"), note.optString("text"))
            "gone" -> Notifier.cancel(this, id)
        }
    }

    override fun onRegistered(token: String) {
        Core.setPushToken(token)
    }
}
