package us.peerb.app

import android.app.Application
import android.util.Log
import androidx.lifecycle.DefaultLifecycleObserver
import androidx.lifecycle.LifecycleOwner
import androidx.lifecycle.ProcessLifecycleOwner
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.google.firebase.messaging.FirebaseMessaging

class PeerbusApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Notifier.channels(this)
        Core.init(this)
        startPush()
        // On the mesh only while the app is on screen; pushes cover the rest.
        ProcessLifecycleOwner.get().lifecycle.addObserver(object : DefaultLifecycleObserver {
            override fun onStart(owner: LifecycleOwner) {
                Core.hold()
            }

            override fun onStop(owner: LifecycleOwner) {
                Core.release()
            }
        })
    }

    /** Firebase from firebase.properties, baked in at build time; without it, no pushes. */
    private fun startPush() {
        if (BuildConfig.FIREBASE_APP_ID.isEmpty()) {
            Core.pushState(Core.Push.Unconfigured)
            return
        }
        val options = FirebaseOptions.Builder()
            .setApplicationId(BuildConfig.FIREBASE_APP_ID)
            .setApiKey(BuildConfig.FIREBASE_API_KEY)
            .setProjectId(BuildConfig.FIREBASE_PROJECT_ID)
            .setGcmSenderId(BuildConfig.FIREBASE_SENDER_ID)
            .build()
        FirebaseApp.initializeApp(this, options)
        // The token arrives in PushService.onRegistered when it's new or changed.
        FirebaseMessaging.getInstance().register()
            .addOnSuccessListener { Core.pushRegistered() }
            .addOnFailureListener {
                Log.w("peerbus", "push registration", it)
                Core.pushState(Core.Push.Failed)
            }
    }
}
