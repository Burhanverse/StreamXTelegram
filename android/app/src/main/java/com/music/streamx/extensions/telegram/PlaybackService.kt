package com.music.streamx.extensions.telegram

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Context
import android.content.Intent
import android.graphics.BitmapFactory
import android.os.Binder
import android.os.Build
import android.os.IBinder
import androidx.core.app.NotificationCompat
import androidx.media3.common.MediaItem
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer

class PlaybackService : Service() {

    companion object {
        private const val CHANNEL_ID = "streamx_playback_channel"
        private const val NOTIFICATION_ID = 1001
    }

    private val binder = LocalBinder()
    var player: ExoPlayer? = null
        private set

    inner class LocalBinder : Binder() {
        val service: PlaybackService
            get() = this@PlaybackService
    }

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()

        val notification = createNotification("StreamX Engine Ready")
        startForeground(NOTIFICATION_ID, notification)

        player = ExoPlayer.Builder(this).build().apply {
            repeatMode = Player.REPEAT_MODE_OFF
            playWhenReady = true
        }
    }

    fun playStream(url: String) {
        val mediaItem = MediaItem.fromUri(url)
        player?.setMediaItem(mediaItem)
        player?.prepare()
        player?.play()
    }

    fun seekTo(positionMs: Long) {
        player?.seekTo(positionMs)
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL_ID,
                "StreamX Audio Playback",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Foreground playback channel for StreamX"
            }
            val manager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
            manager.createNotificationChannel(channel)
        }
    }

    private fun createNotification(contentText: String): Notification {
        val largeIconBitmap = try {
            BitmapFactory.decodeResource(resources, R.mipmap.ic_launcher)
        } catch (_: Exception) {
            null
        }

        val builder = NotificationCompat.Builder(this, CHANNEL_ID)
            .setContentTitle("StreamX Engine")
            .setContentText(contentText)
            .setSmallIcon(R.drawable.ic_notification)
            .setOngoing(true)
            .setPriority(NotificationCompat.PRIORITY_LOW)

        if (largeIconBitmap != null) {
            builder.setLargeIcon(largeIconBitmap)
        }

        return builder.build()
    }

    override fun onBind(intent: Intent?): IBinder = binder

    override fun onDestroy() {
        player?.release()
        player = null
        super.onDestroy()
    }
}
