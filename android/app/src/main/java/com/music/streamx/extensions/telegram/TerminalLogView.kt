package com.music.streamx.extensions.telegram

import android.annotation.SuppressLint
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.graphics.Color
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.text.SpannableStringBuilder
import android.text.Spanned
import android.text.style.ForegroundColorSpan
import android.text.style.StyleSpan
import android.util.AttributeSet
import android.util.TypedValue
import android.view.Gravity
import android.view.MotionEvent
import android.view.View
import android.widget.Button
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.core.widget.NestedScrollView
import java.util.concurrent.ConcurrentLinkedQueue

/**
 * TerminalLogView provides a high-fidelity, live terminal console viewport
 * displaying real-time engine, IPC, and MTProto logs with ANSI/level syntax coloring.
 * 
 * Features:
 * - Isolated, non-conflicting scrolling with outer containers.
 * - Smooth batching of rapid log events to maintain high frame-rates.
 * - Reliable Follow mode that pins to the newest entries without focus jumping.
 * - Automatic pause of Follow mode when user manually scrolls up to inspect logs.
 */
class TerminalLogView @JvmOverloads constructor(
    context: Context,
    attrs: AttributeSet? = null,
    defStyleAttr: Int = 0
) : LinearLayout(context, attrs, defStyleAttr) {

    private val logTextView: TextView
    private val scrollView: NestedScrollView
    private val followToggleBtn: Button
    private val logSpans = SpannableStringBuilder()
    private var isAutoScroll = true
    private var isUserTouching = false
    private var lineCount = 0
    private val maxLines = 500

    private val pendingLogs = ConcurrentLinkedQueue<String>()
    private val flushRunnable = Runnable { flushPendingLogs() }

    var onRestartListener: (() -> Unit)? = null

    private val logListener: (String) -> Unit = { line ->
        post {
            appendLog(line)
        }
    }

    init {
        orientation = VERTICAL
        layoutParams = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT)

        // 1. Header Action Toolbar (Compact MD3 controls)
        val header = LinearLayout(context).apply {
            orientation = HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL or Gravity.END
            setPadding(0, dpToPx(4), 0, dpToPx(8))
            layoutParams = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT)
        }

        // Restart Button
        val restartBtn = createSmallButton(context, "Restart", "#27272A", "#FFFFFF") {
            onRestartListener?.invoke()
        }
        header.addView(restartBtn)

        val spacer1 = View(context).apply { layoutParams = LayoutParams(dpToPx(6), 1) }
        header.addView(spacer1)

        // Clear Button
        val clearBtn = createSmallButton(context, "Clear", "#1E1E22", "#A1A1AA") {
            clearConsole()
        }
        header.addView(clearBtn)

        val spacer2 = View(context).apply { layoutParams = LayoutParams(dpToPx(6), 1) }
        header.addView(spacer2)

        // Copy Button
        val copyBtn = createSmallButton(context, "Copy", "#1E1E22", "#A1A1AA") {
            copyConsole()
        }
        header.addView(copyBtn)

        val spacer3 = View(context).apply { layoutParams = LayoutParams(dpToPx(6), 1) }
        header.addView(spacer3)

        // Follow Toggle Button
        followToggleBtn = createSmallButton(context, "Follow: ON", "#005322", "#6BFB91") {
            val newAutoScroll = !isAutoScroll
            updateFollowState(newAutoScroll)
            if (newAutoScroll) {
                scrollToBottom()
            }
        }
        header.addView(followToggleBtn)

        addView(header)

        // 2. Terminal Container with MD3 rounded corners and deep surface tint
        val terminalContainer = LinearLayout(context).apply {
            orientation = VERTICAL
            val bg = GradientDrawable().apply {
                setColor(Color.parseColor("#0B0C0E"))
                cornerRadius = dpToPx(12).toFloat()
                setStroke(dpToPx(1), Color.parseColor("#282D36"))
            }
            background = bg
            layoutParams = LayoutParams(LayoutParams.MATCH_PARENT, dpToPx(240))
        }

        scrollView = NestedScrollView(context).apply {
            layoutParams = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.MATCH_PARENT)
            isFillViewport = true
            isNestedScrollingEnabled = false
            setPadding(dpToPx(10), dpToPx(8), dpToPx(10), dpToPx(8))
            isVerticalScrollBarEnabled = true
        }

        // Intercept touches so inner terminal scrolls smoothly without outer page interference
        setupTouchAndScrollHandling()

        logTextView = TextView(context).apply {
            typeface = Typeface.MONOSPACE
            textSize = 11.5f
            setTextColor(Color.parseColor("#C4C6D0"))
            setLineSpacing(dpToPx(2).toFloat(), 1.0f)
            isFocusable = false
            isFocusableInTouchMode = false
            setTextIsSelectable(false)
            setHorizontallyScrolling(false)
            layoutParams = LayoutParams(LayoutParams.MATCH_PARENT, LayoutParams.WRAP_CONTENT)
        }

        scrollView.addView(logTextView)
        terminalContainer.addView(scrollView)
        addView(terminalContainer)

        // Initial welcome lines
        appendLogLineInternal("[INFO] StreamX Telegram Audio Engine Console initialized.")
        appendLogLineInternal("[INFO] Waiting for engine events and StreamX IPC calls...")
        logTextView.text = logSpans
    }

    @SuppressLint("ClickableViewAccessibility")
    private fun setupTouchAndScrollHandling() {
        scrollView.setOnTouchListener { v, event ->
            when (event.actionMasked) {
                MotionEvent.ACTION_DOWN -> {
                    isUserTouching = true
                    v.parent?.requestDisallowInterceptTouchEvent(true)
                }
                MotionEvent.ACTION_MOVE -> {
                    v.parent?.requestDisallowInterceptTouchEvent(true)
                }
                MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL -> {
                    isUserTouching = false
                    v.parent?.requestDisallowInterceptTouchEvent(false)
                }
            }
            false
        }

        scrollView.setOnScrollChangeListener { _, _, scrollY, _, _ ->
            if (isUserTouching) {
                val maxScroll = (logTextView.height - (scrollView.height - scrollView.paddingTop - scrollView.paddingBottom)).coerceAtLeast(0)
                val atBottom = scrollY >= maxScroll - dpToPx(24)
                if (!atBottom && isAutoScroll) {
                    updateFollowState(false)
                } else if (atBottom && !isAutoScroll) {
                    updateFollowState(true)
                }
            }
        }
    }

    private fun updateFollowState(enabled: Boolean) {
        isAutoScroll = enabled
        followToggleBtn.text = if (enabled) "Follow: ON" else "Follow: OFF"
        followToggleBtn.setTextColor(if (enabled) Color.parseColor("#6BFB91") else Color.parseColor("#8E9199"))
        (followToggleBtn.background as? GradientDrawable)?.setColor(
            if (enabled) Color.parseColor("#005322") else Color.parseColor("#1E1E22")
        )
    }

    fun attach() {
        LogBuffer.addListener(logListener)
        pendingLogs.clear()
        removeCallbacks(flushRunnable)
        logSpans.clear()
        lineCount = 0
        val history = LogBuffer.getAll()
        if (history.isEmpty()) {
            appendLogLineInternal("[INFO] StreamX Telegram Audio Engine Console initialized.")
            appendLogLineInternal("[INFO] Ready. Trigger an engine action or sync to observe live logs.")
        } else {
            history.forEach { appendLogLineInternal(it) }
        }
        logTextView.text = logSpans
        if (isAutoScroll) {
            scrollToBottom()
        }
    }

    fun detach() {
        LogBuffer.removeListener(logListener)
        pendingLogs.clear()
        removeCallbacks(flushRunnable)
    }

    fun clearConsole() {
        pendingLogs.clear()
        removeCallbacks(flushRunnable)
        LogBuffer.clear()
        logSpans.clear()
        lineCount = 0
        logTextView.text = ""
        appendLogLineInternal("[INFO] Console cleared.")
        logTextView.text = logSpans
        scrollView.scrollTo(0, 0)
    }

    private fun copyConsole() {
        val clipboard = context.getSystemService(Context.CLIPBOARD_SERVICE) as? ClipboardManager
        val clip = ClipData.newPlainText("StreamX Telegram Engine Logs", logTextView.text.toString())
        clipboard?.setPrimaryClip(clip)
        Toast.makeText(context, "Terminal logs copied to clipboard", Toast.LENGTH_SHORT).show()
    }

    private fun appendLog(line: String) {
        pendingLogs.add(line)
        removeCallbacks(flushRunnable)
        // Batch log updates over a ~25ms frame window to eliminate UI micro-stutters
        postDelayed(flushRunnable, 25)
    }

    private fun flushPendingLogs() {
        if (pendingLogs.isEmpty()) return
        var updated = false

        while (true) {
            val line = pendingLogs.poll() ?: break
            appendLogLineInternal(line)
            updated = true
        }

        if (updated) {
            logTextView.text = logSpans
            if (isAutoScroll) {
                scrollToBottom()
            }
        }
    }

    private fun appendLogLineInternal(line: String) {
        if (lineCount >= maxLines) {
            val firstNewline = logSpans.indexOf('\n')
            if (firstNewline >= 0 && firstNewline < logSpans.length) {
                logSpans.delete(0, firstNewline + 1)
                lineCount--
            }
        }

        val start = logSpans.length
        logSpans.append(line).append("\n")
        val end = logSpans.length

        applyLineColor(line, start, end)
        lineCount++
    }

    private fun applyLineColor(line: String, start: Int, end: Int) {
        val upper = line.uppercase()
        val color = when {
            upper.contains("[ERROR]") || upper.contains("FAIL") || upper.contains("FATAL") -> Color.parseColor("#F85149")
            upper.contains("[WARN") || upper.contains("WARNING") -> Color.parseColor("#FFA726")
            upper.contains("[PROGRESS]") || upper.contains("SYNCED") || upper.contains("COMPLETED") || upper.contains("RUNNING") -> Color.parseColor("#3FB950")
            upper.contains("[PROVIDER]") || upper.contains("IPC:") -> Color.parseColor("#58A6FF")
            upper.contains("[STREAM]") || upper.contains("OPENFILE") -> Color.parseColor("#D2A8FF")
            upper.contains("[WAKELOCK]") -> Color.parseColor("#79C0FF")
            upper.contains("[UI]") -> Color.parseColor("#BC8CFF")
            upper.contains("[DEBUG]") -> Color.parseColor("#8B949E")
            else -> Color.parseColor("#C9D1D9")
        }

        logSpans.setSpan(ForegroundColorSpan(color), start, end, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)

        val tagEnd = line.indexOf(']')
        if (tagEnd > 0 && tagEnd + start < end) {
            logSpans.setSpan(StyleSpan(Typeface.BOLD), start, start + tagEnd + 1, Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        }
    }

    private fun scrollToBottom() {
        scrollView.post {
            val maxScroll = (logTextView.bottom + scrollView.paddingBottom) - scrollView.height
            if (maxScroll > 0) {
                scrollView.scrollTo(0, maxScroll)
            }
        }
    }

    private fun createSmallButton(
        context: Context,
        label: String,
        bgColorHex: String,
        textColorHex: String,
        onClick: (Button) -> Unit
    ): Button {
        return Button(context).apply {
            text = label
            textSize = 11f
            setTextColor(Color.parseColor(textColorHex))
            val bg = GradientDrawable().apply {
                setColor(Color.parseColor(bgColorHex))
                cornerRadius = dpToPx(14).toFloat()
                setStroke(dpToPx(1), Color.parseColor("#282D36"))
            }
            background = bg
            minHeight = dpToPx(28)
            minimumHeight = dpToPx(28)
            setPadding(dpToPx(12), dpToPx(4), dpToPx(12), dpToPx(4))
            layoutParams = LayoutParams(LayoutParams.WRAP_CONTENT, dpToPx(30))
            isAllCaps = false
            setOnClickListener { onClick(this) }
        }
    }

    private fun dpToPx(dp: Int): Int {
        return TypedValue.applyDimension(
            TypedValue.COMPLEX_UNIT_DIP,
            dp.toFloat(),
            context.resources.displayMetrics
        ).toInt()
    }
}
