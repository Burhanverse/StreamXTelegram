package com.music.streamx.extensions.telegram

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.graphics.BitmapFactory
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import androidx.core.app.NotificationCompat

class BackgroundWorkerService : Service() {

    private var wakeLock: PowerManager.WakeLock? = null

    companion object {
        private const val CHANNEL_ID = "background_worker_channel"
        private const val NOTIFICATION_ID = 8743
        private const val WAKE_LOCK_TAG = "StreamX::BackgroundWorkerWakeLock"
        private const val PREFS_NAME = "background_worker_prefs"
        private const val KEY_ENABLED = "worker_enabled"

        const val ACTION_START = "START_SERVICE"
        const val ACTION_STOP = "STOP_SERVICE"

        private var isServiceRunning = false
        private val listeners = mutableListOf<(Boolean) -> Unit>()

        fun isRunning(): Boolean = isServiceRunning

        fun isPersistedEnabled(context: Context): Boolean {
            val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            return prefs.getBoolean(KEY_ENABLED, false)
        }

        fun setPersistedEnabled(context: Context, enabled: Boolean) {
            val prefs = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            prefs.edit().putBoolean(KEY_ENABLED, enabled).apply()
        }

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

        fun start(context: Context) {
            setPersistedEnabled(context, true)
            val startIntent = Intent(context, BackgroundWorkerService::class.java).apply {
                action = ACTION_START
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(startIntent)
            } else {
                context.startService(startIntent)
            }
        }

        fun stop(context: Context) {
            setPersistedEnabled(context, false)
            val stopIntent = Intent(context, BackgroundWorkerService::class.java).apply {
                action = ACTION_STOP
            }
            context.startService(stopIntent)
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

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_START -> startWorker()
            ACTION_STOP -> stopWorker()
            else -> startWorker()
        }
        return START_STICKY
    }

    private fun startWorker() {
        createNotificationChannel()

        val stopIntent = Intent(this, BackgroundWorkerService::class.java).apply {
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

        val largeIconBitmap = try {
            BitmapFactory.decodeResource(resources, R.mipmap.ic_launcher)
        } catch (_: Exception) {
            null
        }

        val notificationBuilder = NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("Background Worker Active")
            .setContentText("Keeping the CPU awake for background operations.")
            .setSmallIcon(R.drawable.ic_notification)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setContentIntent(configPendingIntent)
            .addAction(android.R.drawable.ic_menu_close_clear_cancel, "Stop Worker", stopPendingIntent)

        if (largeIconBitmap != null) {
            notificationBuilder.setLargeIcon(largeIconBitmap)
        }

        val notification: Notification = notificationBuilder.build()

        startForeground(NOTIFICATION_ID, notification)
        acquireWakeLock()

        isServiceRunning = true
        notifyListeners(true)
        LogBuffer.add("WAKELOCK", "Background Worker & CPU WakeLock active")
    }

    private fun acquireWakeLock() {
        if (wakeLock == null) {
            val powerManager = getSystemService(Context.POWER_SERVICE) as PowerManager
            wakeLock = powerManager.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKE_LOCK_TAG).apply {
                setReferenceCounted(false)
            }
        }

        if (wakeLock?.isHeld == false) {
            wakeLock?.acquire()
        }
    }

    private fun stopWorker() {
        releaseWakeLock()
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
        isServiceRunning = false
        notifyListeners(false)
        LogBuffer.add("WAKELOCK", "Background Worker & CPU WakeLock stopped")
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
                "Background Tasks",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Channel for long running continuous background execution."
                setShowBadge(false)
            }
            val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    override fun onDestroy() {
        super.onDestroy()
        releaseWakeLock()
        isServiceRunning = false
        notifyListeners(false)
    }
}

