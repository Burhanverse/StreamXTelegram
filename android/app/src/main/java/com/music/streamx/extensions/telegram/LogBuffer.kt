package com.music.streamx.extensions.telegram

import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * LogBuffer stores a rolling buffer of recent engine logs and broadcasts
 * new entries to active UI listeners (e.g. ConfigActivity's live terminal).
 */
object LogBuffer {
    private const val MAX_LOGS = 600
    private val buffer = ArrayDeque<String>(MAX_LOGS)
    private val listeners = mutableSetOf<(String) -> Unit>()
    private val timeFormat = SimpleDateFormat("HH:mm:ss", Locale.ROOT)

    @Synchronized
    fun add(tag: String, message: String) {
        val timestamp = timeFormat.format(Date())
        val line = "[$timestamp] [$tag] $message"
        if (buffer.size >= MAX_LOGS) {
            buffer.removeFirst()
        }
        buffer.add(line)
        val targets = listeners.toList()
        targets.forEach { it(line) }
    }

    @Synchronized
    fun addRaw(line: String) {
        if (buffer.size >= MAX_LOGS) {
            buffer.removeFirst()
        }
        buffer.add(line)
        val targets = listeners.toList()
        targets.forEach { it(line) }
    }

    @Synchronized
    fun getAll(): List<String> = buffer.toList()

    @Synchronized
    fun clear() {
        buffer.clear()
    }

    @Synchronized
    fun addListener(listener: (String) -> Unit) {
        listeners.add(listener)
    }

    @Synchronized
    fun removeListener(listener: (String) -> Unit) {
        listeners.remove(listener)
    }
}

