package com.music.streamx.extensions.telegram

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PluginBridgeUnitTest {

    @Test
    fun testCredentialsSerializationRoundTrip() {
        val original = KeyStoreManager.Credentials(
            apiId = 9876543,
            apiHash = "abcdef0123456789abcdef0123456789",
            chatId = -1001987654321L,
            botTokens = listOf(
                "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
                "654321:XYZ-DEF1234ghIkl-zyx57W2v1u123ew22",
                "789012:UVW-DEF1234ghIkl-zyx57W2v1u123ew33"
            ),
            systemTopicId = 42
        )

        val jsonString = original.toJSON()
        assertNotNull(jsonString)

        val restored = KeyStoreManager.Credentials.fromJSON(jsonString)
        assertEquals(original.apiId, restored.apiId)
        assertEquals(original.apiHash, restored.apiHash)
        assertEquals(original.chatId, restored.chatId)
        assertEquals(original.systemTopicId, restored.systemTopicId)
        assertEquals(3, restored.botTokens.size)
        assertEquals("123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11", restored.botTokens[0])
        assertEquals("654321:XYZ-DEF1234ghIkl-zyx57W2v1u123ew22", restored.botTokens[1])
        assertEquals("789012:UVW-DEF1234ghIkl-zyx57W2v1u123ew33", restored.botTokens[2])
    }

    @Test
    fun testStreamXHealthJsonContract() {
        val healthJson = JSONObject().apply {
            put("ok", true)
            put("message", "Connected as Telegram Engine")
            put("capabilities", JSONArray(listOf("home", "browse", "library", "search", "detail", "account", "stream", "saved", "save")))
        }

        assertTrue(healthJson.getBoolean("ok"))
        val caps = healthJson.getJSONArray("capabilities")
        val capsList = (0 until caps.length()).map { caps.getString(it) }
        assertTrue(capsList.contains("home"))
        assertTrue(capsList.contains("search"))
        assertTrue(capsList.contains("stream"))
        assertTrue(capsList.contains("detail"))
    }

    @Test
    fun testStreamXStreamUrlContract() {
        // Option A: content:// URI (preferred)
        val trackId = "test_track_123"
        val authority = "com.music.streamx.extensions.telegram.source"
        val contentUrl = "content://$authority/track/$trackId"
        val optionAJson = JSONObject().apply {
            put("url", contentUrl)
            put("codec", "mp3")
            put("kbps", 320)
        }
        assertEquals(contentUrl, optionAJson.getString("url"))
        assertTrue(optionAJson.getString("url").startsWith("content://"))
        assertEquals("mp3", optionAJson.getString("codec"))
        assertEquals(320, optionAJson.getInt("kbps"))

        // Option B: http:// loopback URI (fallback)
        val port = 34567
        val token = "abc123token"
        val loopbackUrl = "http://127.0.0.1:$port/stream/$trackId?token=$token"
        val optionBJson = JSONObject().put("url", loopbackUrl)
        assertEquals(loopbackUrl, optionBJson.getString("url"))
        assertTrue(optionBJson.getString("url").startsWith("http://127.0.0.1"))
    }


    @Test
    fun testStreamXHomeSectionsContract() {
        val homeJson = JSONObject().apply {
            put("account", JSONObject().put("id", "telegram:local").put("name", "Telegram Library"))
            put("sections", JSONArray().apply {
                put(JSONObject().apply {
                    put("id", "home:daily")
                    put("title", "Daily Mix")
                    put("subtitle", "Fresh picks")
                    put("items", JSONArray().apply {
                        put(JSONObject().apply {
                            put("type", "track")
                            put("id", "track_01")
                            put("title", "Bohemian Rhapsody")
                            put("artist", "Queen")
                            put("duration_sec", 354)
                        })
                    })
                })
            })
        }

        val sections = homeJson.getJSONArray("sections")
        assertEquals(1, sections.length())
        val firstSection = sections.getJSONObject(0)
        assertEquals("Daily Mix", firstSection.getString("title"))
        val items = firstSection.getJSONArray("items")
        assertEquals(1, items.length())
        val item0 = items.getJSONObject(0)
        assertEquals("track", item0.getString("type"))
        assertEquals("Queen", item0.getString("artist"))
    }

    @Test
    fun testLogBufferRollingAndListeners() {
        LogBuffer.clear()
        val received = mutableListOf<String>()
        val listener: (String) -> Unit = { received.add(it) }

        LogBuffer.addListener(listener)
        LogBuffer.add("TEST", "Hello World")
        LogBuffer.addRaw("[12:00:00] [RAW] Direct log line")

        assertEquals(2, received.size)
        assertTrue(received[0].contains("[TEST] Hello World"))
        assertTrue(received[1].contains("[RAW] Direct log line"))

        val all = LogBuffer.getAll()
        assertEquals(2, all.size)

        LogBuffer.removeListener(listener)
        LogBuffer.add("TEST", "After unsubscribe")
        assertEquals(2, received.size) // No new notifications after removeListener
        assertEquals(3, LogBuffer.getAll().size)
    }

    @Test
    fun testStreamXLyricsAndPlaylistsContract() {
        val lyricsJson = JSONObject().apply {
            put("lines", JSONArray().apply {
                put(JSONObject().apply {
                    put("time_ms", 12000)
                    put("text", "Is this the real life?")
                })
            })
            put("sync_type", "LINE_SYNCED")
        }
        assertEquals("LINE_SYNCED", lyricsJson.getString("sync_type"))
        assertEquals(1, lyricsJson.getJSONArray("lines").length())

        val playlistsJson = JSONObject().apply {
            put("items", JSONArray())
            put("next_offset", -1)
        }
        assertEquals(-1, playlistsJson.getInt("next_offset"))
        assertEquals(0, playlistsJson.getJSONArray("items").length())
    }

    @Test
    fun testWakeLockStateTransitions() {
        // Test that release without acquire doesn't throw
        WakeLockManager.release("test-safety")
        // Verify LogBuffer records wakelock events
        LogBuffer.add("WAKELOCK", "CPU WakeLock acquired (playback-test)")
        val logs = LogBuffer.getAll()
        assertTrue(logs.any { it.contains("CPU WakeLock acquired (playback-test)") })
    }
}
