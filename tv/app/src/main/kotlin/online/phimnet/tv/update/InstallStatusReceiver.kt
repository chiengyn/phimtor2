package online.phimnet.tv.update

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import androidx.core.content.IntentCompat
import online.phimnet.tv.PhimnetApp

/**
 * Receives the PackageInstaller session's outcome (see Updater.commit).
 *
 * STATUS_PENDING_USER_ACTION carries the system's own confirm screen, which must
 * be shown: a self-update of a sideloaded app always needs the person's say-so,
 * since the app was not installed by this app. On success the system kills
 * this process to replace it (see [Updater]), so there is nothing to do here.
 */
class InstallStatusReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent) {
        val updater = (context.applicationContext as PhimnetApp).graph.updater
        when (val status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)) {
            PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                val confirm = IntentCompat.getParcelableExtra(intent, Intent.EXTRA_INTENT, Intent::class.java)
                if (confirm == null) {
                    updater.onInstallFailed(status, "no confirm intent")
                    return
                }
                context.startActivity(confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            }
            PackageInstaller.STATUS_SUCCESS -> Unit
            PackageInstaller.STATUS_FAILURE_ABORTED -> updater.onInstallCancelled()
            else -> updater.onInstallFailed(status, intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE))
        }
    }
}
