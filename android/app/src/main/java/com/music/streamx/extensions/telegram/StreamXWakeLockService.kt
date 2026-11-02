package com.music.streamx.extensions.telegram

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import androidx.core.app.NotificationCompat

/**
 * StreamXWakeLockService provides a persistent CPU WakeLock and
 * Foreground Service to prevent Android Doze / App Standby from suspending MTProto
 * sockets and audio streaming when the screen is turned off or while multitasking.
 */
class StreamXWakeLockService : Service() {

    companion object {
        const val ACTION_START = "com.music.streamx.extensions.telegram.action.START_WAKELOCK"
        const val ACTION_STOP = "com.music.streamx.extensions.telegram.action.STOP_WAKELOCK"
        const val ACTION_TOGGLE = "com.music.streamx.extensions.telegram.action.TOGGLE_WAKELOCK"

        private const val CHANNEL_ID = "streamx_engine_wakelock_channel"
        private const val NOTIFICATION_ID = 2002
        private const val WAKELOCK_TAG = "streamx:telegram:playback_stream"
        private const val PREFS_NAME = "streamx_wakelock_prefs"
        private const val KEY_WAKELOCK_ENABLED = "wakelock_enabled"

        private var wakeLock: PowerManager.WakeLock? = null
        private var isServiceRunning = false
        private val listeners = mutableListOf<(Boolean) -> Unit>()

        fun addListener(listener: (Boolean) -> Unit) {
            synchronized(listeners) {
                listeners.add(listener)
                listener(isRunning())
            }
        }

        fun removeListener(listener: (Boolean) -> Unit) {
            synchronized(listeners) {
                listeners.remove(listener)
            }
        }

        fun isRunning(): Boolean = isServiceRunning

        fun isPersistedEnabled(context: Context): Boolean {
            val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            return prefs.getBoolean(KEY_WAKELOCK_ENABLED, false)
        }

        fun setPersistedEnabled(context: Context, enabled: Boolean) {
            val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            prefs.edit().putBoolean(KEY_WAKELOCK_ENABLED, enabled).apply()
        }

        fun start(context: Context) {
            setPersistedEnabled(context, true)
            val intent = Intent(context, StreamXWakeLockService::class.java).apply {
                action = ACTION_START
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stop(context: Context) {
            setPersistedEnabled(context, false)
            val intent = Intent(context, StreamXWakeLockService::class.java).apply {
                action = ACTION_STOP
            }
            context.startService(intent)
        }

        fun toggle(context: Context) {
            if (isRunning()) {
                stop(context)
            } else {
                start(context)
            }
        }

        private fun notifyListeners(running: Boolean) {
            synchronized(listeners) {
                listeners.forEach { it(running) }
            }
        }
    }

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        val action = intent?.action ?: ACTION_START

        when (action) {
            ACTION_STOP -> {
                releaseWakeLock()
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                isServiceRunning = false
                notifyListeners(false)
                LogBuffer.add("WAKELOCK", "Streaming WakeLock released (Stopped)")
                return START_NOT_STICKY
            }
            ACTION_TOGGLE -> {
                if (isServiceRunning) {
                    releaseWakeLock()
                    stopForeground(STOP_FOREGROUND_REMOVE)
                    stopSelf()
                    isServiceRunning = false
                    notifyListeners(false)
                    LogBuffer.add("WAKELOCK", "Streaming WakeLock (OFF)")
                    return START_NOT_STICKY
                } else {
                    acquireWakeLock()
                }
            }
            else -> {
                acquireWakeLock()
            }
        }

        val notification = buildForegroundNotification()
        startForeground(NOTIFICATION_ID, notification)
        isServiceRunning = true
        notifyListeners(true)
        LogBuffer.add("WAKELOCK", "Streaming WakeLock (ON)")

        return START_STICKY
    }

    private fun acquireWakeLock() {
        if (wakeLock == null) {
            val pm = getSystemService(Context.POWER_SERVICE) as? PowerManager
            wakeLock = pm?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKELOCK_TAG)?.apply {
                setReferenceCounted(false)
            }
        }
        if (wakeLock?.isHeld != true) {
            wakeLock?.acquire()
        }
    }

    private fun releaseWakeLock() {
        try {
            if (wakeLock?.isHeld == true) {
                wakeLock?.release()
            }
        } catch (_: Exception) {}
        wakeLock = null
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "StreamX Streaming Engine WakeLock",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Keeps CPU awake and MTProto connection alive during screen-off audio streaming"
                setShowBadge(false)
            }
            val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    private fun buildForegroundNotification(): Notification {
        val stopIntent = Intent(this, StreamXWakeLockService::class.java).apply {
            action = ACTION_STOP
        }
        val stopPendingIntent = PendingIntent.getService(
            this,
            1,
            stopIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) PendingIntent.FLAG_IMMUTABLE else 0)
        )

        val configIntent = Intent(this, ConfigActivity::class.java).apply {
            flags = Intent.FLAG_ACTIVITY_SINGLE_TOP
        }
        val configPendingIntent = PendingIntent.getActivity(
            this,
            0,
            configIntent,
            PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) PendingIntent.FLAG_IMMUTABLE else 0)
        )

        return NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("StreamX Telegram Audio Engine")
            .setContentText("WakeLock active • Off-screen streaming protected")
            .setSmallIcon(android.R.drawable.ic_media_play)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setContentIntent(configPendingIntent)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Release WakeLock", stopPendingIntent)
            .build()
    }

    override fun onDestroy() {
        releaseWakeLock()
        isServiceRunning = false
        notifyListeners(false)
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null
}

