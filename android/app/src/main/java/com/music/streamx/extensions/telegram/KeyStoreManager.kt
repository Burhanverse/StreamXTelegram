package com.music.streamx.extensions.telegram

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import org.json.JSONArray
import org.json.JSONObject
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/**
 * KeyStoreManager provides hardware-backed AES-256-GCM encryption for storing
 * sensitive Telegram credentials (API ID, API Hash, Chat ID, Bot Tokens) in SharedPreferences.
 */
object KeyStoreManager {

    private const val ANDROID_KEYSTORE = "AndroidKeyStore"
    private const val KEY_ALIAS = "streamx_engine_secrets"
    private const val PREFS_NAME = "streamx_engine_secure_prefs"
    private const val KEY_ENCRYPTED_CONFIG = "encrypted_config_payload"
    private const val GCM_IV_LENGTH = 12
    private const val GCM_TAG_LENGTH = 128

    data class Credentials(
        val apiId: Int,
        val apiHash: String,
        val chatId: Long,
        val botTokens: List<String>,
        val systemTopicId: Int = 0
    ) {
        fun toJSON(): String {
            val json = JSONObject()
            json.put("api_id", apiId)
            json.put("api_hash", apiHash)
            json.put("chat_id", chatId)
            val tokensArray = JSONArray()
            botTokens.forEach { tokensArray.put(it) }
            json.put("bot_tokens", tokensArray)
            json.put("bot_token", botTokens.firstOrNull() ?: "")
            val workerArr = JSONArray()
            if (botTokens.size > 1) {
                for (i in 1 until botTokens.size) {
                    workerArr.put(botTokens[i])
                }
            }
            json.put("worker_tokens", workerArr)
            json.put("system_topic_id", systemTopicId)
            return json.toString()
        }

        companion object {
            fun fromJSON(jsonStr: String): Credentials {
                val json = JSONObject(jsonStr)
                val tokens = mutableListOf<String>()
                val arr = json.optJSONArray("bot_tokens")
                if (arr != null) {
                    for (i in 0 until arr.length()) {
                        val token = arr.optString(i, "").trim()
                        if (token.isNotEmpty()) tokens.add(token)
                    }
                }
                if (tokens.isEmpty()) {
                    val primary = json.optString("bot_token", "").trim()
                    if (primary.isNotEmpty()) tokens.add(primary)
                    val workers = json.optJSONArray("worker_tokens")
                    if (workers != null) {
                        for (i in 0 until workers.length()) {
                            val token = workers.optString(i, "").trim()
                            if (token.isNotEmpty()) tokens.add(token)
                        }
                    }
                }
                return Credentials(
                    apiId = json.optInt("api_id", 0),
                    apiHash = json.optString("api_hash", ""),
                    chatId = json.optLong("chat_id", 0L),
                    botTokens = tokens,
                    systemTopicId = json.optInt("system_topic_id", 0)
                )
            }
        }
    }


    private fun getOrCreateSecretKey(): SecretKey {
        val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        val existingKey = keyStore.getKey(KEY_ALIAS, null) as? SecretKey
        if (existingKey != null) {
            return existingKey
        }

        val keyGenerator = KeyGenerator.getInstance(
            KeyProperties.KEY_ALGORITHM_AES,
            ANDROID_KEYSTORE
        )
        val spec = KeyGenParameterSpec.Builder(
            KEY_ALIAS,
            KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT
        )
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
            .setKeySize(256)
            .build()

        keyGenerator.init(spec)
        return keyGenerator.generateKey()
    }

    fun encrypt(plainText: String): String {
        val secretKey = getOrCreateSecretKey()
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.ENCRYPT_MODE, secretKey)
        val iv = cipher.iv
        val cipherText = cipher.doFinal(plainText.toByteArray(Charsets.UTF_8))

        val combined = ByteArray(iv.size + cipherText.size)
        System.arraycopy(iv, 0, combined, 0, iv.size)
        System.arraycopy(cipherText, 0, combined, iv.size, cipherText.size)

        return Base64.encodeToString(combined, Base64.NO_WRAP)
    }

    fun decrypt(encryptedBase64: String): String {
        val combined = Base64.decode(encryptedBase64, Base64.NO_WRAP)
        if (combined.size < GCM_IV_LENGTH) {
            throw IllegalArgumentException("Invalid encrypted payload size")
        }

        val iv = ByteArray(GCM_IV_LENGTH)
        val cipherText = ByteArray(combined.size - GCM_IV_LENGTH)
        System.arraycopy(combined, 0, iv, 0, GCM_IV_LENGTH)
        System.arraycopy(combined, GCM_IV_LENGTH, cipherText, 0, cipherText.size)

        val secretKey = getOrCreateSecretKey()
        val cipher = Cipher.getInstance("AES/GCM/NoPadding")
        cipher.init(Cipher.DECRYPT_MODE, secretKey, GCMParameterSpec(GCM_TAG_LENGTH, iv))

        return String(cipher.doFinal(cipherText), Charsets.UTF_8)
    }

    fun saveCredentials(context: Context, credentials: Credentials) {
        val json = credentials.toJSON()
        val encrypted = encrypt(json)
        context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            .edit()
            .putString(KEY_ENCRYPTED_CONFIG, encrypted)
            .apply()
    }

    fun loadCredentials(context: Context): Credentials? {
        val encrypted = context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            .getString(KEY_ENCRYPTED_CONFIG, null) ?: return null
        return try {
            val json = decrypt(encrypted)
            Credentials.fromJSON(json)
        } catch (e: Exception) {
            null
        }
    }

    fun clearCredentials(context: Context) {
        context.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)
            .edit()
            .remove(KEY_ENCRYPTED_CONFIG)
            .apply()
    }

    fun get(context: Context, key: String): String? {
        val creds = loadCredentials(context) ?: return null
        return when (key) {
            "api_id" -> if (creds.apiId > 0) creds.apiId.toString() else null
            "api_hash" -> creds.apiHash.ifEmpty { null }
            "chat_id" -> if (creds.chatId != 0L) creds.chatId.toString() else null
            "bot_tokens" -> creds.botTokens.joinToString(", ").ifEmpty { null }
            "system_topic_id" -> creds.systemTopicId.toString()
            else -> null
        }
    }

    fun put(context: Context, key: String, value: String) {
        val existing = loadCredentials(context) ?: Credentials(0, "", 0L, emptyList(), 1)
        val updated = when (key) {
            "api_id" -> existing.copy(apiId = value.toIntOrNull() ?: 0)
            "api_hash" -> existing.copy(apiHash = value)
            "chat_id" -> existing.copy(chatId = value.toLongOrNull() ?: 0L)
            "bot_tokens" -> existing.copy(botTokens = value.split(",").map { it.trim() }.filter { it.isNotEmpty() })
            "system_topic_id" -> existing.copy(systemTopicId = value.toIntOrNull() ?: 1)
            else -> existing
        }
        saveCredentials(context, updated)
    }

    fun remove(context: Context, key: String) {
        val existing = loadCredentials(context) ?: return
        val updated = when (key) {
            "api_id" -> existing.copy(apiId = 0)
            "api_hash" -> existing.copy(apiHash = "")
            "chat_id" -> existing.copy(chatId = 0L)
            "bot_tokens" -> existing.copy(botTokens = emptyList())
            "system_topic_id" -> existing.copy(systemTopicId = 1)
            else -> existing
        }
        saveCredentials(context, updated)
    }
}
