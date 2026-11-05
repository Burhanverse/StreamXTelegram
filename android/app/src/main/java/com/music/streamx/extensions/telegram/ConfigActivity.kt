package com.music.streamx.extensions.telegram

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.graphics.Color
import android.graphics.drawable.GradientDrawable
import android.os.Build
import android.os.Bundle
import android.view.View
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowInsetsCompat
import androidx.lifecycle.lifecycleScope
import com.google.android.material.bottomsheet.BottomSheetBehavior
import com.google.android.material.bottomsheet.BottomSheetDialog
import com.music.streamx.extensions.telegram.databinding.ActivityConfigBinding
import com.music.streamx.extensions.telegram.databinding.ActivitySettingsBinding
import engine.Engine
import engine.EventCallback
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject

/**
 * ConfigActivity provides the primary dashboard for the StreamX Telegram extension,
 * featuring live diagnostic terminal, engine lifecycle management, channel sync,
 * WakeLock controls, and a popup configuration bottom sheet for credentials.
 */
class ConfigActivity : AppCompatActivity() {

    private lateinit var binding: ActivityConfigBinding

    private var isSyncing = false
    private var isBackingUp = false
    private var isRestoring = false

    private val wakeLockListener: (Boolean) -> Unit = { isHeld ->
        runOnUiThread {
            updateWakeLockUI(isHeld)
        }
    }

    private val notificationPermissionLauncher = registerForActivityResult(
        ActivityResultContracts.RequestPermission()
    ) { isGranted ->
        if (isGranted) {
            BackgroundWorkerService.start(this)
        } else {
            Toast.makeText(this, "Notification permission is required for background worker", Toast.LENGTH_SHORT).show()
        }
    }

    private fun startWorkerWithPermissionCheck() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            if (ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) == PackageManager.PERMISSION_GRANTED) {
                BackgroundWorkerService.start(this)
            } else {
                notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
        } else {
            BackgroundWorkerService.start(this)
        }
    }

    private fun toggleWorkerWithPermissionCheck() {
        if (BackgroundWorkerService.isRunning()) {
            BackgroundWorkerService.stop(this)
        } else {
            startWorkerWithPermissionCheck()
        }
    }

    private val eventCallback = object : EventCallback {
        override fun onProgress(status: String?, current: Long, total: Long) {
            runOnUiThread {
                val msg = "$status ($current/$total)"
                binding.syncStatusText.text = "Sync status: $msg"
                LogBuffer.add("PROGRESS", msg)
            }
        }

        override fun onError(message: String?) {
            runOnUiThread {
                val err = message ?: "Unknown error"
                binding.syncStatusText.text = "Error: $err"
                LogBuffer.add("ERROR", err)
            }
        }

        override fun onLog(line: String?) {
            runOnUiThread {
                if (!line.isNullOrEmpty()) {
                    LogBuffer.add("MOBILE", line)
                }
            }
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        binding = ActivityConfigBinding.inflate(layoutInflater)
        setContentView(binding.root)

        // Handle system bar insets so content is cleanly below the status bar
        ViewCompat.setOnApplyWindowInsetsListener(binding.rootScrollView) { view, insets ->
            val bars = insets.getInsets(WindowInsetsCompat.Type.systemBars())
            view.setPadding(0, bars.top, 0, bars.bottom)
            insets
        }

        // Initialize logging and mobile callback
        Engine.setEventCallback(eventCallback)
        LogBuffer.add("UI", "StreamX Telegram Dashboard initialized.")

        // Attach live terminal view
        binding.terminalView.attach()
        binding.terminalView.onRestartListener = {
            restartEngine()
        }

        // Setup top gear icon for configuration
        binding.gearBtn.setOnClickListener {
            showSettingsBottomSheet()
        }

        // Setup sync, backup, and restore controls
        binding.syncBtn.setOnClickListener {
            performSync()
        }
        binding.rescanBtn.setOnClickListener {
            confirmRescanFromScratch()
        }
        binding.backupBtn.setOnClickListener {
            performBackup()
        }
        binding.restoreBtn.setOnClickListener {
            performRestore()
        }

        // Setup WakeLock toggle button
        binding.wakeLockToggleBtn.setOnClickListener {
            toggleWorkerWithPermissionCheck()
        }
        BackgroundWorkerService.addListener(wakeLockListener)

        // Auto-restore WakeLock if enabled previously
        if (BackgroundWorkerService.isPersistedEnabled(this) && !BackgroundWorkerService.isRunning()) {
            startWorkerWithPermissionCheck()
        }

        // Check if opened from intent with popup request
        if (intent.getBooleanExtra("OPEN_POPUP", false)) {
            showSettingsBottomSheet()
        }

        binding.footerSubtitle.text = "v${BuildConfig.VERSION_NAME} • On-Device Engine"

        // Start diagnostic status polling loop
        startEngineMonitoring()
    }

    override fun onResume() {
        super.onResume()
        updateEngineStatusUI()
    }

    override fun onDestroy() {
        BackgroundWorkerService.removeListener(wakeLockListener)
        binding.terminalView.detach()
        super.onDestroy()
    }

    private fun startEngineMonitoring() {
        lifecycleScope.launch {
            while (isActive) {
                updateEngineStatusUI()
                delay(2000)
            }
        }
    }

    private fun updateEngineStatusUI() {
        val port = Engine.getPort().toInt()
        val health = Engine.health()
        val authority = BuildConfig.PROVIDER_AUTHORITY

        if (port > 0) {
            val isWatcher = Engine.isWatcherRunning()
            val watcherTag = if (isWatcher) " • Watcher: ON" else ""
            binding.statusPill.text = "● RUNNING :$port"
            binding.statusPill.setTextColor(Color.parseColor("#4ADE80"))
            val pillBg = GradientDrawable().apply {
                setColor(Color.parseColor("#002D12"))
                setStroke(dpToPx(1), Color.parseColor("#005322"))
                cornerRadius = dpToPx(12).toFloat()
            }
            binding.statusPill.background = pillBg

            binding.statusDetails.text = "Host: 127.0.0.1:$port • Health:\n$health\nAuthority:\n$authority$watcherTag"
        } else {
            binding.statusPill.text = "● STOPPED"
            binding.statusPill.setTextColor(Color.parseColor("#FFA726"))
            val pillBg = GradientDrawable().apply {
                setColor(Color.parseColor("#2D1D00"))
                setStroke(dpToPx(1), Color.parseColor("#5A3A00"))
                cornerRadius = dpToPx(12).toFloat()
            }
            binding.statusPill.background = pillBg

            binding.statusDetails.text = "Host: 127.0.0.1 (Offline) • Health:\n$health\nAuthority:\n$authority"
        }
    }

    private fun updateWakeLockUI(isHeld: Boolean) {
        if (isHeld) {
            binding.wakeLockToggleBtn.text = "⚡ WakeLock: ACTIVE"
            binding.wakeLockToggleBtn.setTextColor(Color.parseColor("#4ADE80"))
            binding.wakeLockToggleBtn.setBackgroundColor(Color.parseColor("#002D12"))
            binding.wakeLockToggleBtn.strokeColor = android.content.res.ColorStateList.valueOf(Color.parseColor("#005322"))
        } else {
            binding.wakeLockToggleBtn.text = "⚡ WakeLock: OFF"
            binding.wakeLockToggleBtn.setTextColor(Color.parseColor("#A1A1AA"))
            binding.wakeLockToggleBtn.setBackgroundColor(Color.parseColor("#1E1E22"))
            binding.wakeLockToggleBtn.strokeColor = android.content.res.ColorStateList.valueOf(Color.parseColor("#27272A"))
        }
    }

    private fun performSync() {
        if (isSyncing) return
        val chatId = getSavedChatId()
        if (chatId == 0L) {
            Toast.makeText(this, "Please configure Channel ID first", Toast.LENGTH_SHORT).show()
            showSettingsBottomSheet()
            return
        }

        isSyncing = true
        binding.syncBtn.isEnabled = false
        binding.syncBtn.text = "Syncing Channel..."
        binding.syncStatusText.text = "Sync status: Starting channel scan..."
        LogBuffer.add("SYNC", "Starting manual Telegram channel sync for chat $chatId...")

        lifecycleScope.launch(Dispatchers.IO) {
            try {
                Engine.syncChannel(chatId)
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Sync status: Completed successfully"
                    LogBuffer.add("SYNC", "Telegram channel sync completed successfully.")
                    Toast.makeText(this@ConfigActivity, "Channel synced successfully", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Sync error: ${e.message}"
                    LogBuffer.add("ERROR", "Sync failed: ${e.message}")
                    Toast.makeText(this@ConfigActivity, "Sync failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            } finally {
                withContext(Dispatchers.Main) {
                    isSyncing = false
                    binding.syncBtn.isEnabled = true
                    binding.syncBtn.text = "Sync Channel / Catch-up"
                }
            }
        }
    }

    private fun confirmRescanFromScratch() {
        if (isSyncing || isBackingUp || isRestoring) return
        val chatId = getSavedChatId()
        if (chatId == 0L) {
            Toast.makeText(this, "Please configure Channel ID first", Toast.LENGTH_SHORT).show()
            showSettingsBottomSheet()
            return
        }

        com.google.android.material.dialog.MaterialAlertDialogBuilder(this)
            .setTitle("Delete Database?")
            .setMessage("This will wipe all locally indexed tracks, playlists, and cached sync state, then re-scan the entire Telegram channel and all its forum topics from message 1.\n\nAre you sure you want to proceed?")
            .setPositiveButton("Reset & Rescan") { _, _ ->
                performRescanFromScratch(chatId)
            }
            .setNegativeButton("Cancel", null)
            .show()
    }

    private fun performRescanFromScratch(chatId: Long) {
        if (isSyncing) return
        isSyncing = true
        binding.syncBtn.isEnabled = false
        binding.rescanBtn.isEnabled = false
        binding.rescanBtn.text = "Rescanning From Scratch..."
        binding.syncStatusText.text = "Sync status: Resetting DB and scanning all topics from scratch..."
        LogBuffer.add("SYNC", "Starting complete Telegram database reset and re-scan from scratch for chat $chatId...")

        lifecycleScope.launch(Dispatchers.IO) {
            try {
                Engine.rescanFromScratch(chatId)
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Sync status: Deteled & scanned successfully"
                    LogBuffer.add("SYNC", "Database deteled and all topics scanned successfully.")
                    Toast.makeText(this@ConfigActivity, "Database deteled and scanned successfully", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Rescan error: ${e.message}"
                    LogBuffer.add("ERROR", "Rescan from scratch failed: ${e.message}")
                    Toast.makeText(this@ConfigActivity, "Rescan failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            } finally {
                withContext(Dispatchers.Main) {
                    isSyncing = false
                    binding.syncBtn.isEnabled = true
                    binding.rescanBtn.isEnabled = true
                    binding.rescanBtn.text = "Detele DB & Rescan From Scratch"
                }
            }
        }
    }

    private fun performBackup() {
        if (isBackingUp) return
        val chatId = getSavedChatId()
        val topicId = getSavedTopicId()

        isBackingUp = true
        binding.backupBtn.isEnabled = false
        binding.backupBtn.text = "Backing up..."
        binding.syncStatusText.text = "Sync status: Exporting database snapshot..."
        LogBuffer.add("BACKUP", "Initiating SQLite backup to Telegram topic $topicId...")

        lifecycleScope.launch(Dispatchers.IO) {
            try {
                Engine.backupDatabase(chatId, topicId)
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Backup status: Uploaded snapshot to Telegram"
                    LogBuffer.add("BACKUP", "Database snapshot uploaded to Telegram.")
                    Toast.makeText(this@ConfigActivity, "Database backed up successfully", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Backup error: ${e.message}"
                    LogBuffer.add("ERROR", "Backup failed: ${e.message}")
                    Toast.makeText(this@ConfigActivity, "Backup failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            } finally {
                withContext(Dispatchers.Main) {
                    isBackingUp = false
                    binding.backupBtn.isEnabled = true
                    binding.backupBtn.text = "Backup DB"
                }
            }
        }
    }

    private fun performRestore() {
        if (isRestoring) return
        val chatId = getSavedChatId()
        val topicId = getSavedTopicId()

        isRestoring = true
        binding.restoreBtn.isEnabled = false
        binding.restoreBtn.text = "Restoring..."
        binding.syncStatusText.text = "Sync status: Restoring snapshot from Telegram..."
        LogBuffer.add("RESTORE", "Initiating SQLite restore from Telegram topic $topicId...")

        lifecycleScope.launch(Dispatchers.IO) {
            try {
                Engine.restoreDatabase(chatId, topicId)
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Restore status: Restored database snapshot"
                    LogBuffer.add("RESTORE", "Database snapshot restored from Telegram.")
                    Toast.makeText(this@ConfigActivity, "Database restored successfully", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    binding.syncStatusText.text = "Restore error: ${e.message}"
                    LogBuffer.add("ERROR", "Restore failed: ${e.message}")
                    Toast.makeText(this@ConfigActivity, "Restore failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            } finally {
                withContext(Dispatchers.Main) {
                    isRestoring = false
                    binding.restoreBtn.isEnabled = true
                    binding.restoreBtn.text = "Restore DB"
                }
            }
        }
    }

    private fun restartEngine() {
        LogBuffer.add("ENGINE", "Restarting StreamX Telegram engine...")
        lifecycleScope.launch(Dispatchers.IO) {
            try {
                Engine.stop()
                val configJson = loadConfigJSON()
                val filesDir = applicationContext.filesDir.absolutePath
                val cacheDir = applicationContext.cacheDir.absolutePath
                val result = Engine.start(filesDir, cacheDir, configJson)
                withContext(Dispatchers.Main) {
                    LogBuffer.add("ENGINE", "Engine restarted on port ${result.port}.")
                    updateEngineStatusUI()
                    Toast.makeText(this@ConfigActivity, "Engine restarted on port ${result.port}", Toast.LENGTH_SHORT).show()
                }
            } catch (e: Exception) {
                withContext(Dispatchers.Main) {
                    LogBuffer.add("ERROR", "Failed to restart engine: ${e.message}")
                    Toast.makeText(this@ConfigActivity, "Restart failed: ${e.message}", Toast.LENGTH_LONG).show()
                }
            }
        }
    }

    private fun showSettingsBottomSheet() {
        val sheetDialog = BottomSheetDialog(this, R.style.StreamX_BottomSheetDialogTheme)
        val sheetBinding = ActivitySettingsBinding.inflate(layoutInflater)
        sheetDialog.setContentView(sheetBinding.root)

        // Expand bottom sheet fully
        sheetDialog.behavior.state = BottomSheetBehavior.STATE_EXPANDED
        sheetDialog.behavior.skipCollapsed = true

        // Prefill existing credentials
        val prefs = getSharedPreferences("telegram_config", Context.MODE_PRIVATE)
        val apiId = KeyStoreManager.get(this, "api_id") ?: prefs.getString("api_id", "") ?: ""
        val apiHash = KeyStoreManager.get(this, "api_hash") ?: prefs.getString("api_hash", "") ?: ""
        val chatId = KeyStoreManager.get(this, "chat_id") ?: prefs.getString("chat_id", "") ?: ""
        val botTokens = KeyStoreManager.get(this, "bot_tokens") ?: prefs.getString("bot_tokens", "") ?: ""
        val systemTopic = KeyStoreManager.get(this, "system_topic_id") ?: prefs.getString("system_topic_id", "1") ?: "1"

        sheetBinding.apiIdInput.setText(apiId)
        sheetBinding.apiHashInput.setText(apiHash)
        sheetBinding.chatIdInput.setText(chatId)
        sheetBinding.botTokensInput.setText(botTokens)
        sheetBinding.systemTopicInput.setText(systemTopic)

        sheetBinding.backBtn.setOnClickListener {
            sheetDialog.dismiss()
        }

        sheetBinding.clearBtn.setOnClickListener {
            sheetBinding.apiIdInput.setText("")
            sheetBinding.apiHashInput.setText("")
            sheetBinding.chatIdInput.setText("")
            sheetBinding.botTokensInput.setText("")
            sheetBinding.systemTopicInput.setText("1")

            KeyStoreManager.remove(this, "api_id")
            KeyStoreManager.remove(this, "api_hash")
            KeyStoreManager.remove(this, "chat_id")
            KeyStoreManager.remove(this, "bot_tokens")
            KeyStoreManager.remove(this, "system_topic_id")
            prefs.edit().clear().apply()

            LogBuffer.add("CONFIG", "Credentials cleared.")
            Toast.makeText(this, "Credentials cleared", Toast.LENGTH_SHORT).show()
        }

        sheetBinding.saveBtn.setOnClickListener {
            val newApiId = sheetBinding.apiIdInput.text.toString().trim()
            val newApiHash = sheetBinding.apiHashInput.text.toString().trim()
            val newChatId = sheetBinding.chatIdInput.text.toString().trim()
            val newBotTokens = sheetBinding.botTokensInput.text.toString().trim()
            val newTopicId = sheetBinding.systemTopicInput.text.toString().trim().ifEmpty { "1" }

            KeyStoreManager.put(this, "api_id", newApiId)
            KeyStoreManager.put(this, "api_hash", newApiHash)
            KeyStoreManager.put(this, "chat_id", newChatId)
            KeyStoreManager.put(this, "bot_tokens", newBotTokens)
            KeyStoreManager.put(this, "system_topic_id", newTopicId)

            prefs.edit()
                .putString("api_id", newApiId)
                .putString("api_hash", newApiHash)
                .putString("chat_id", newChatId)
                .putString("bot_tokens", newBotTokens)
                .putString("system_topic_id", newTopicId)
                .apply()

            LogBuffer.add("CONFIG", "Credentials saved and encrypted in Android KeyStore.")
            Toast.makeText(this, "Credentials saved successfully", Toast.LENGTH_SHORT).show()

            sheetDialog.dismiss()
            restartEngine()
        }

        sheetDialog.show()
    }

    private fun getSavedChatId(): Long {
        val prefs = getSharedPreferences("telegram_config", Context.MODE_PRIVATE)
        val raw = KeyStoreManager.get(this, "chat_id") ?: prefs.getString("chat_id", "0") ?: "0"
        return raw.toLongOrNull() ?: 0L
    }

    private fun getSavedTopicId(): Long {
        val prefs = getSharedPreferences("telegram_config", Context.MODE_PRIVATE)
        val raw = KeyStoreManager.get(this, "system_topic_id") ?: prefs.getString("system_topic_id", "1") ?: "1"
        return raw.toLongOrNull() ?: 1L
    }

    private fun loadConfigJSON(): String {
        val prefs = getSharedPreferences("telegram_config", Context.MODE_PRIVATE)
        val apiIdStr = KeyStoreManager.get(this, "api_id") ?: prefs.getString("api_id", "0") ?: "0"
        val apiHash = KeyStoreManager.get(this, "api_hash") ?: prefs.getString("api_hash", "") ?: ""
        val chatIdStr = KeyStoreManager.get(this, "chat_id") ?: prefs.getString("chat_id", "0") ?: "0"
        val botTokensStr = KeyStoreManager.get(this, "bot_tokens") ?: prefs.getString("bot_tokens", "") ?: ""
        val topicIdStr = KeyStoreManager.get(this, "system_topic_id") ?: prefs.getString("system_topic_id", "1") ?: "1"

        val tokens = botTokensStr.split(",").map { it.trim() }.filter { it.isNotEmpty() }
        val primaryToken = tokens.firstOrNull() ?: ""
        val workerTokens = if (tokens.size > 1) tokens.drop(1) else emptyList()

        val json = JSONObject().apply {
            put("api_id", apiIdStr.toIntOrNull() ?: 0)
            put("api_hash", apiHash)
            put("chat_id", chatIdStr.toLongOrNull() ?: 0L)
            put("bot_token", primaryToken)
            put("system_topic_id", topicIdStr.toIntOrNull() ?: 1)
            put("worker_tokens", JSONArray(workerTokens))
            put("bot_tokens", JSONArray(tokens))
            put("auth_token", "streamx-secret-session-token")
        }
        return json.toString()
    }

    private fun dpToPx(dp: Int): Int {
        return (dp * resources.displayMetrics.density).toInt()
    }
}
