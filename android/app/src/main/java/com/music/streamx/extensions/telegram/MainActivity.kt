package com.music.streamx.extensions.telegram

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.os.Build
import android.os.Bundle
import android.os.IBinder
import android.util.Log
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import androidx.appcompat.app.AppCompatActivity
import androidx.lifecycle.lifecycleScope
import androidx.media3.common.PlaybackException
import androidx.media3.common.Player
import engine.Engine
import engine.StartResult
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

class MainActivity : AppCompatActivity() {

    companion object {
        private const val TAG = "TelegramMainActivity"
    }

    private var playbackService: PlaybackService? = null
    private var isBound = false
    private var startResult: StartResult? = null
    private var streamUrl: String = ""

    private var requestStartTime: Long = 0
    private var seekStartTime: Long = 0

    private val serviceConnection = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName?, binder: IBinder?) {
            val localBinder = binder as PlaybackService.LocalBinder
            playbackService = localBinder.service
            isBound = true
            setupPlayerListeners()
            startSpikeStream()
        }

        override fun onServiceDisconnected(name: ComponentName?) {
            playbackService = null
            isBound = false
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        val rootLayout = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(32, 48, 32, 48)
        }

        val settingsBtn = Button(this).apply {
            text = "Open Telegram Plugin Settings"
            setOnClickListener {
                startActivity(Intent(this@MainActivity, ConfigActivity::class.java))
            }
        }
        rootLayout.addView(settingsBtn)

        val statusView = TextView(this).apply {
            textSize = 14f
            setPadding(0, 24, 0, 24)
            text = "Initializing StreamX Telegram Extension..."
        }
        rootLayout.addView(statusView)
        setContentView(rootLayout)

        lifecycleScope.launch {
            // 1. Check ALAC native support
            val alacReport = AlacVerification.checkNativeSupport()
            logAndDisplay(statusView, "ALAC Native Decoder: ${if (alacReport.isSupportedNatively) "Supported (${alacReport.codecName})" else "Unsupported"}\n${alacReport.details}")

            // 2. Start Go Engine
            withContext(Dispatchers.IO) {
                try {
                    val configJson = """{"api_id": 0, "api_hash": "", "bot_token": ""}"""
                    startResult = Engine.start(
                        filesDir.absolutePath,
                        cacheDir.absolutePath,
                        configJson
                    )

                    // Register a known spike test track
                    val mockData = ByteArray(5 * 1024 * 1024) { (it % 256).toByte() }
                    Engine.registerMockTrack(
                        "spike_track_01",
                        "Spike Demo Track",
                        "Gotd Audio Engine",
                        240,
                        "audio/mpeg",
                        mockData
                    )

                    streamUrl = Engine.getStreamURL("spike_track_01")
                } catch (e: Exception) {
                    Log.e(TAG, "Failed to start Go engine", e)
                }
            }

            logAndDisplay(statusView, "Engine started on port ${startResult?.port}\nToken: [REDACTED]\nStream URL: $streamUrl")

            // 3. Start & Bind PlaybackService
            val intent = Intent(this@MainActivity, PlaybackService::class.java)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                startForegroundService(intent)
            } else {
                startService(intent)
            }
            bindService(intent, serviceConnection, Context.BIND_AUTO_CREATE)
        }
    }

    private fun setupPlayerListeners() {
        playbackService?.player?.addListener(object : Player.Listener {
            override fun onPlaybackStateChanged(playbackState: Int) {
                when (playbackState) {
                    Player.STATE_READY -> {
                        if (requestStartTime > 0) {
                            val ttfb = System.currentTimeMillis() - requestStartTime
                            Log.i(TAG, "Measured First-Byte / Ready Latency: ${ttfb}ms")
                            requestStartTime = 0

                            // Trigger seek test after 2 seconds of playback
                            lifecycleScope.launch {
                                delay(2000)
                                testSeek()
                            }
                        }
                        if (seekStartTime > 0) {
                            val seekLatency = System.currentTimeMillis() - seekStartTime
                            Log.i(TAG, "Measured Seek Latency: ${seekLatency}ms")
                            seekStartTime = 0
                        }
                    }
                    Player.STATE_ENDED -> Log.i(TAG, "Playback ended")
                    Player.STATE_BUFFERING -> Log.d(TAG, "Buffering chunk...")
                    Player.STATE_IDLE -> Log.d(TAG, "Player idle")
                }
            }

            override fun onPlayerError(error: PlaybackException) {
                Log.e(TAG, "ExoPlayer error: ${error.errorCodeName}", error)
            }
        })
    }

    private fun startSpikeStream() {
        if (streamUrl.isNotEmpty()) {
            requestStartTime = System.currentTimeMillis()
            playbackService?.playStream(streamUrl)
        }
    }

    private fun testSeek() {
        val targetMs = 120_000L // Seek to 2:00 mark
        Log.i(TAG, "Performing seek test to ${targetMs}ms")
        seekStartTime = System.currentTimeMillis()
        playbackService?.seekTo(targetMs)
    }

    private fun logAndDisplay(view: TextView, message: String) {
        Log.i(TAG, message)
        runOnUiThread {
            view.append("\n\n$message")
        }
    }

    override fun onDestroy() {
        if (isBound) {
            unbindService(serviceConnection)
            isBound = false
        }
        Engine.stop()
        super.onDestroy()
    }
}
