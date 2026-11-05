package com.music.streamx.extensions.telegram

import android.content.ContentProvider
import android.content.ContentValues
import android.content.Context
import android.database.Cursor
import android.net.Uri
import android.os.Binder
import android.os.Bundle
import android.os.Handler
import android.os.HandlerThread
import android.os.ParcelFileDescriptor
import android.os.PowerManager
import android.os.Process
import android.os.storage.StorageManager
import android.system.ErrnoException
import android.system.OsConstants
import android.util.Log
import engine.Engine
import engine.EventCallback
import org.json.JSONArray
import org.json.JSONObject
import java.io.BufferedReader
import java.io.FileNotFoundException
import java.io.InputStreamReader
import java.net.HttpURLConnection
import java.net.URL
import java.net.URLEncoder

/**
 * StreamXProvider implements the StreamX extension ContentProvider contract
 * (com.music.streamx.extensions.telegram.source).
 *
 * It bridges incoming ContentProvider call() actions to the local Go HTTP engine
 * running on 127.0.0.1:<port>/api/streamx/... and serves seekable audio streams
 * via content:// URIs and StorageManager.openProxyFileDescriptor.
 */
class StreamXProvider : ContentProvider() {

    companion object {
        private const val TAG = "StreamXTelegramProvider"
        const val AUTHORITY = BuildConfig.PROVIDER_AUTHORITY
        const val ARTWORK_DIR_NAME = "artwork"

        fun getArtworkDirName(): String = try {
            Engine.getArtworkDirName()
        } catch (_: Throwable) {
            ARTWORK_DIR_NAME
        }
    }

    private val proxyThread by lazy {
        HandlerThread("TelegramStreamProxy", Process.THREAD_PRIORITY_AUDIO).apply { start() }
    }
    private val proxyHandler by lazy { Handler(proxyThread.looper) }

    override fun onCreate(): Boolean {

        Log.i(TAG, "Initializing StreamXProvider with authority $AUTHORITY")
        val ctx = context?.applicationContext ?: return true

        // Clean up legacy covers directory (Prompt 5 requirement: single shared artwork path, delete legacy path)
        try {
            val oldCoversDir = java.io.File(ctx.filesDir, "covers")
            if (oldCoversDir.exists()) {
                oldCoversDir.deleteRecursively()
            }
        } catch (e: Exception) {
            Log.w(TAG, "Failed to clean legacy covers directory: ${e.message}")
        }

        // Register event listener for engine notifications
        Engine.setEventCallback(object : EventCallback {
            override fun onProgress(status: String?, current: Long, total: Long) {
                Log.d(TAG, "Engine Progress: status=$status, current=$current, total=$total")
                LogBuffer.add("PROGRESS", "$status ($current/$total)")
            }

            override fun onError(message: String?) {
                Log.e(TAG, "Engine Error: $message")
                LogBuffer.add("ERROR", message ?: "Unknown error")
            }

            override fun onLog(line: String?) {
                if (!line.isNullOrBlank()) {
                    Log.d(TAG, line)
                    LogBuffer.addRaw(line)
                }
            }
        })


        // Register screen state receiver for diagnostic tagging (screen-on vs screen-off)
        try {
            val powerManager = ctx.getSystemService(Context.POWER_SERVICE) as? PowerManager
            Engine.setScreenOn(powerManager?.isInteractive ?: true)
            val screenFilter = android.content.IntentFilter().apply {
                addAction(android.content.Intent.ACTION_SCREEN_ON)
                addAction(android.content.Intent.ACTION_SCREEN_OFF)
            }
            ctx.registerReceiver(object : android.content.BroadcastReceiver() {
                override fun onReceive(context: Context?, intent: android.content.Intent?) {
                    when (intent?.action) {
                        android.content.Intent.ACTION_SCREEN_ON -> Engine.setScreenOn(true)
                        android.content.Intent.ACTION_SCREEN_OFF -> Engine.setScreenOn(false)
                    }
                }
            }, screenFilter)
        } catch (e: Exception) {
            Log.w(TAG, "Failed to register screen state receiver: ${e.message}")
        }

        // Background start if credentials exist
        Thread {
            ensureEngineStarted()
        }.start()

        return true
    }

    private fun ensureEngineStarted(): Boolean {
        val port = Engine.getPort()
        if (port > 0) {
            return true
        }

        val ctx = context?.applicationContext ?: return false
        val creds = KeyStoreManager.loadCredentials(ctx)
        val configJson = creds?.toJSON() ?: ""

        val filesDir = ctx.filesDir.absolutePath
        val cacheDir = ctx.cacheDir.absolutePath

        return try {
            val result = Engine.start(filesDir, cacheDir, configJson)
            Log.i(TAG, "Engine started on HTTP 127.0.0.1:${result.port}, HTTPS 127.0.0.1:${result.httpsPort}")
            true
        } catch (e: Exception) {
            Log.e(TAG, "Failed to start engine: ${e.message}")
            false
        }
    }

    override fun call(method: String, arg: String?, extras: Bundle?): Bundle {
        val callingUid = Binder.getCallingUid()
        val callerPkg = callingPackage

        // 1. Strict caller verification before starting engine or returning any data/token
        val ctx = context ?: throw IllegalStateException("Provider context is null")
        CallerVerifier.verifyCaller(ctx, callingUid, callerPkg)
        LogBuffer.add("PROVIDER", "IPC: $method (caller: $callerPkg)")

        val bundle = extras ?: Bundle()
        val jsonString: String

        try {
            ensureEngineStarted()
            val port = Engine.getPort()
            val token = Engine.getToken()

            if (port <= 0) {
                jsonString = when (method) {
                    "streamx.health.v1" -> JSONObject().put("ok", false)
                        .put("message", "Telegram Engine is stopped. Open settings to configure.")
                        .put("capabilities", JSONArray(listOf("home", "browse", "library", "search", "detail", "account", "stream")))
                        .toString()
                    "streamx.account.v1" -> JSONObject().put("id", "telegram:unconfigured")
                        .put("name", "Not Configured")
                        .put("thumbnail_url", "")
                        .toString()
                    "streamx.home.v1", "streamx.browse.v1", "streamx.library.v1" -> JSONObject()
                        .put("account", JSONObject().put("id", "telegram:unconfigured").put("name", "Not Configured"))
                        .put("sections", JSONArray())
                        .toString()
                    "streamx.search.v1" -> JSONObject().put("items", JSONArray()).toString()
                    "streamx.detail.v1" -> JSONObject().put("title", "Empty").put("kind", "collection").put("items", JSONArray()).toString()
                    "streamx.stream.v1" -> JSONObject().put("url", "").toString()
                    "streamx.saved.v1", "streamx.save.v1" -> JSONObject().put("saved", false).toString()
                    "streamx.lyrics.v1" -> "{}"
                    "streamx.playlists.v1" -> "{\"items\":[],\"next_offset\":-1}"
                    "streamx.radio.v1" -> "{\"title\":\"Radio\",\"sections\":[]}"
                    "streamx.search_landing.v1" -> "{\"title\":\"Search\",\"full_search\":true,\"sections\":[]}"
                    "streamx.search_page.v1" -> "{\"items\":[]}"
                    "streamx.history.v1" -> "{\"items\":[]}"
                    "streamx.preferences.v1" -> "{}"
                    "streamx.plan.v1" -> "{\"plan\":\"free\"}"
                    "streamx.playback_route.v1" -> "{\"route\":\"native\"}"
                    "streamx.library_item_actions.v1" -> "{\"actions\":[]}"
                    "streamx.playback.v1" -> "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0}"
                    "streamx.create_playlist.v1" -> "{\"id\":\"pl_unconfigured\",\"warning\":\"Engine stopped\"}"
                    "streamx.add_to_playlist.v1", "streamx.remove_from_playlist.v1" -> "{\"ok\":false}"
                    else -> throw IllegalArgumentException("Unsupported operation: $method")
                }
            } else {
                val baseURL = "http://127.0.0.1:$port"

                jsonString = when (method) {
                    "streamx.health.v1" -> httpGet("$baseURL/api/streamx/health?token=$token")
                    "streamx.account.v1" -> httpGet("$baseURL/api/streamx/account?token=$token")
                    "streamx.home.v1" -> httpGet("$baseURL/api/streamx/home?token=$token")
                    "streamx.browse.v1" -> httpGet("$baseURL/api/streamx/browse?token=$token")
                    "streamx.library.v1" -> httpGet("$baseURL/api/streamx/library?token=$token")
                    "streamx.search.v1" -> {
                        val query = bundle.getString("query", "")
                        val limit = bundle.getInt("limit", 20).coerceIn(1, 40)
                        val encodedQuery = URLEncoder.encode(query, "UTF-8")
                        httpGet("$baseURL/api/streamx/search?query=$encodedQuery&limit=$limit&token=$token")
                    }
                    "streamx.detail.v1" -> {
                        val kind = bundle.getString("kind", "")
                        val id = bundle.getString("id", "")
                        val offset = bundle.getInt("offset", 0).coerceAtLeast(0)
                        val encodedKind = URLEncoder.encode(kind, "UTF-8")
                        val encodedId = URLEncoder.encode(id, "UTF-8")
                        httpGet("$baseURL/api/streamx/detail?kind=$encodedKind&id=$encodedId&offset=$offset&token=$token")
                    }
                    "streamx.stream.v1" -> {
                        val trackId = bundle.getString("track_id") ?: bundle.getString("id") ?: ""
                        val contentUri = "content://$AUTHORITY/track/$trackId"
                        val quality = bundle.getString("quality")
                        val maxKbps = bundle.getInt("max_kbps", 320)
                        val isLosslessRequested = quality == "lossless"
                        val codec = if (isLosslessRequested) "flac" else "mp3"
                        val kbps = if (isLosslessRequested) null else minOf(320, maxKbps)

                        JSONObject().apply {
                            put("url", contentUri)
                            put("codec", codec)
                            if (kbps != null) {
                                put("kbps", kbps)
                            }
                        }.toString()
                    }
                    "streamx.saved.v1" -> {
                        val kind = bundle.getString("kind", "")
                        val id = bundle.getString("id", "")
                        val encodedKind = URLEncoder.encode(kind, "UTF-8")
                        val encodedId = URLEncoder.encode(id, "UTF-8")
                        httpGet("$baseURL/api/streamx/saved?kind=$encodedKind&id=$encodedId&token=$token")
                    }
                    "streamx.save.v1" -> {
                        val kind = bundle.getString("kind", "")
                        val id = bundle.getString("id", "")
                        val saved = bundle.getBoolean("saved", false)
                        val encodedKind = URLEncoder.encode(kind, "UTF-8")
                        val encodedId = URLEncoder.encode(id, "UTF-8")
                        httpGet("$baseURL/api/streamx/save?kind=$encodedKind&id=$encodedId&saved=$saved&token=$token")
                    }
                    "streamx.lyrics.v1" -> {
                        val trackId = bundle.getString("track_id") ?: bundle.getString("id") ?: ""
                        val encodedId = URLEncoder.encode(trackId, "UTF-8")
                        httpGet("$baseURL/api/streamx/lyrics?track_id=$encodedId&token=$token")
                    }
                    "streamx.playlists.v1" -> {
                        val offset = bundle.getInt("offset", 0).coerceAtLeast(0)
                        val folder = bundle.getString("folder", "")
                        val track = bundle.getString("track", "")
                        val encodedFolder = URLEncoder.encode(folder, "UTF-8")
                        val encodedTrack = URLEncoder.encode(track, "UTF-8")
                        httpGet("$baseURL/api/streamx/playlists?offset=$offset&folder=$encodedFolder&track=$encodedTrack&token=$token")
                    }
                    "streamx.radio.v1" -> "{\"title\":\"Radio\",\"sections\":[]}"
                    "streamx.search_landing.v1" -> "{\"title\":\"Search\",\"full_search\":true,\"sections\":[]}"
                    "streamx.search_page.v1" -> {
                        val query = bundle.getString("query", "")
                        val limit = bundle.getInt("limit", 20).coerceIn(1, 40)
                        val encodedQuery = URLEncoder.encode(query, "UTF-8")
                        httpGet("$baseURL/api/streamx/search?query=$encodedQuery&limit=$limit&token=$token")
                    }
                    "streamx.history.v1" -> "{\"items\":[]}"
                    "streamx.preferences.v1" -> "{}"
                    "streamx.plan.v1" -> "{\"plan\":\"free\"}"
                    "streamx.playback_route.v1" -> "{\"route\":\"native\"}"
                    "streamx.library_item_actions.v1" -> "{\"actions\":[]}"
                    "streamx.playback.v1" -> "{\"state\":\"idle\",\"position_ms\":0,\"duration_ms\":0}"
                    "streamx.create_playlist.v1" -> {
                        val title = bundle.getString("title", "New Playlist")
                        val encodedTitle = URLEncoder.encode(title, "UTF-8")
                        httpGet("$baseURL/api/streamx/create_playlist?title=$encodedTitle&token=$token")
                    }
                    "streamx.add_to_playlist.v1" -> {
                        val id = bundle.getString("id", "")
                        val tracks = bundle.getString("tracks", "[]")
                        val encodedId = URLEncoder.encode(id, "UTF-8")
                        val encodedTracks = URLEncoder.encode(tracks, "UTF-8")
                        httpGet("$baseURL/api/streamx/add_to_playlist?id=$encodedId&tracks=$encodedTracks&token=$token")
                    }
                    "streamx.remove_from_playlist.v1" -> {
                        val id = bundle.getString("id", "")
                        val track = bundle.getString("track", "")
                        val encodedId = URLEncoder.encode(id, "UTF-8")
                        val encodedTrack = URLEncoder.encode(track, "UTF-8")
                        httpGet("$baseURL/api/streamx/remove_from_playlist?id=$encodedId&track=$encodedTrack&token=$token")
                    }
                    else -> throw IllegalArgumentException("Unsupported operation: $method")
                }

            }
        } catch (e: Exception) {
            Log.e(TAG, "Error handling $method: ${e.message}", e)
            val errJson = JSONObject().put("error", e.message ?: "Request failed").toString()
            return Bundle().apply { putString("json", errJson) }
        }

        return Bundle().apply {
            putString("json", jsonString)
        }
    }

    override fun openFile(uri: Uri, mode: String): ParcelFileDescriptor? {
        val callingUid = Binder.getCallingUid()
        val callerPkg = callingPackage
        val ctx = context ?: throw IllegalStateException("Provider context is null")

        val segments = uri.pathSegments
        val isArt = segments.isNotEmpty() && segments[0] == "art"
        val trackId = if (segments.size >= 2) segments[1] else ""

        if (isArt) {
            Log.d(TAG, "ART request received: trackId=$trackId, callerPkg=$callerPkg, uid=$callingUid")
            LogBuffer.add("ART", "Request received: trackId=$trackId, callerPkg=$callerPkg, uid=$callingUid")
        }

        try {
            CallerVerifier.verifyCaller(ctx, callingUid, callerPkg)
        } catch (e: SecurityException) {
            if (isArt) {
                Log.w(TAG, "ART verifier rejection: callerPkg=$callerPkg, uid=$callingUid: ${e.message}")
                LogBuffer.add("ART", "Verifier rejection: callerPkg=$callerPkg, uid=$callingUid: ${e.message}")
            }
            throw e
        }

        if (mode != "r") {
            throw SecurityException("Only read-only mode 'r' is supported, requested: $mode")
        }

        if (segments.size < 2 || (segments[0] != "track" && segments[0] != "art")) {
            throw FileNotFoundException("Unsupported URI path: ${uri.path}")
        }

        if (isArt) {
            val artworkDir = getArtworkDirName()
            val coverFile = java.io.File(ctx.filesDir, "$artworkDir/$trackId.jpg")
            if (coverFile.exists() && coverFile.length() > 0) {
                val bytes = coverFile.length()
                Log.d(TAG, "ART file served: trackId=$trackId, bytes=$bytes")
                LogBuffer.add("ART", "File served: trackId=$trackId, bytes=$bytes")
                return ParcelFileDescriptor.open(coverFile, ParcelFileDescriptor.MODE_READ_ONLY)
            }
            Log.w(TAG, "ART file missing: ${coverFile.absolutePath} (trackId=$trackId)")
            LogBuffer.add("ART", "File missing: ${coverFile.absolutePath} (trackId=$trackId)")
            throw FileNotFoundException("Cover art not found for track: $trackId")
        }

        LogBuffer.add("STREAM", "openFile: trackId=$trackId caller=$callerPkg")

        ensureEngineStarted()

        val trackSize = Engine.getTrackSize(trackId)
        if (trackSize <= 0) {
            throw FileNotFoundException("Track not found or empty: $trackId")
        }

        val storageManager = ctx.getSystemService(Context.STORAGE_SERVICE) as? StorageManager
            ?: throw FileNotFoundException("StorageManager unavailable")

        val handle = try {
            Engine.openStream(trackId)
        } catch (e: Exception) {
            Log.e(TAG, "Failed to open engine stream session for track $trackId: ${e.message}")
            throw FileNotFoundException("Failed to open stream session: ${e.message}")
        }

        // Dedicated HandlerThread per open stream session so preloaded next tracks cannot block current playback
        val streamThread = HandlerThread("StreamProxy-$trackId-$handle", Process.THREAD_PRIORITY_AUDIO).apply { start() }
        val streamHandler = Handler(streamThread.looper)

        WakeLockManager.acquire(ctx, "stream-$trackId")

        return storageManager.openProxyFileDescriptor(
            ParcelFileDescriptor.MODE_READ_ONLY,
            object : android.os.ProxyFileDescriptorCallback() {
                override fun onGetSize(): Long = trackSize

                override fun onRead(offset: Long, size: Int, data: ByteArray): Int {
                    if (offset >= trackSize || size <= 0) {
                        return 0
                    }
                    val pm = ctx.getSystemService(Context.POWER_SERVICE) as? PowerManager
                    if (pm != null) {
                        Engine.setScreenOn(pm.isInteractive)
                    }
                    return try {
                        val chunk = Engine.readStream(handle, offset, size.toLong())
                        if (chunk == null || chunk.isEmpty()) {
                            0
                        } else {
                            val toCopy = minOf(chunk.size, size)
                            System.arraycopy(chunk, 0, data, 0, toCopy)
                            toCopy
                        }
                    } catch (e: Exception) {
                        Log.w(TAG, "Proxy read error for track $trackId (handle $handle) at offset $offset: ${e.message}")
                        throw ErrnoException("onRead", OsConstants.EIO)
                    }
                }

                override fun onRelease() {
                    Log.d(TAG, "Proxy descriptor released for track $trackId (handle $handle)")
                    try {
                        Engine.closeStream(handle)
                    } catch (_: Throwable) {}
                    try {
                        streamThread.quitSafely()
                    } catch (_: Throwable) {}
                    WakeLockManager.release("stream-$trackId")
                }
            },
            streamHandler
        )
    }

    private fun httpGet(urlStr: String): String {
        val url = URL(urlStr)
        val conn = url.openConnection() as HttpURLConnection
        conn.requestMethod = "GET"
        conn.connectTimeout = 4000
        conn.readTimeout = 8000
        conn.setRequestProperty("Accept", "application/json")

        val code = conn.responseCode
        val stream = if (code in 200..299) conn.inputStream else conn.errorStream
        val reader = BufferedReader(InputStreamReader(stream, Charsets.UTF_8))
        val sb = StringBuilder()
        var line: String?
        while (reader.readLine().also { line = it } != null) {
            sb.append(line)
        }
        reader.close()
        conn.disconnect()

        return sb.toString()
    }

    fun getTypeForSegments(segments: List<String>): String? {
        if (segments.isNotEmpty() && segments[0] == "art") {
            return "image/jpeg"
        }
        if (segments.size >= 2 && segments[0] == "track") {
            return "audio/mpeg"
        }
        return null
    }

    override fun getType(uri: Uri): String? {
        return getTypeForSegments(uri.pathSegments)
    }

    override fun query(uri: Uri, projection: Array<out String>?, selection: String?, selectionArgs: Array<out String>?, sortOrder: String?): Cursor? = throw UnsupportedOperationException()
    override fun insert(uri: Uri, values: ContentValues?): Uri? = throw UnsupportedOperationException()
    override fun delete(uri: Uri, selection: String?, selectionArgs: Array<out String>?): Int = throw UnsupportedOperationException()
    override fun update(uri: Uri, values: ContentValues?, selection: String?, selectionArgs: Array<out String>?): Int = throw UnsupportedOperationException()
}

