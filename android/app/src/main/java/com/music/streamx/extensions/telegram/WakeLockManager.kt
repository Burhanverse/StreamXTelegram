package com.music.streamx.extensions.telegram

import android.content.Context
import android.os.PowerManager
import android.util.Log

/**
 * WakeLockManager manages reference-counted CPU WakeLocks (PARTIAL_WAKE_LOCK)
 * to ensure uninterrupted background audio streaming and MTProto sync operations
 * when the screen is turned off.
 */
object WakeLockManager {
    private const val TAG = "StreamXWakeLock"
    private const val LOCK_TAG = "streamx:telegram:playback"

    private var wakeLock: PowerManager.WakeLock? = null
    private val lock = Any()
    private var refCount = 0

    fun acquire(context: Context, reason: String = "streaming") {
        synchronized(lock) {
            refCount++
            Log.d(TAG, "Acquiring WakeLock (reason: $reason, refCount: $refCount)")
            if (wakeLock == null) {
                val powerManager = context.applicationContext.getSystemService(Context.POWER_SERVICE) as? PowerManager
                wakeLock = powerManager?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, LOCK_TAG)?.apply {
                    setReferenceCounted(false)
                }
            }
            if (wakeLock?.isHeld != true) {
                // Safety 30-minute timeout to prevent permanent wakelock leak
                wakeLock?.acquire(30 * 60 * 1000L)
                LogBuffer.add("WAKELOCK", "CPU WakeLock acquired ($reason)")
            }
        }
    }

    fun release(reason: String = "streaming") {
        synchronized(lock) {
            if (refCount > 0) {
                refCount--
            }
            Log.d(TAG, "Releasing WakeLock (reason: $reason, refCount: $refCount)")
            if (refCount == 0 && wakeLock?.isHeld == true) {
                wakeLock?.release()
                LogBuffer.add("WAKELOCK", "CPU WakeLock released ($reason)")
            }
        }
    }
}
