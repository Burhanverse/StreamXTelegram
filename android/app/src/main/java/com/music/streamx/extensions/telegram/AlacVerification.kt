package com.music.streamx.extensions.telegram

import android.media.MediaCodecList
import android.media.MediaFormat
import android.os.Build

data class AlacSupportReport(
    val isSupportedNatively: Boolean,
    val codecName: String?,
    val isHardwareAccelerated: Boolean,
    val isSoftwareOnly: Boolean,
    val details: String
)

object AlacVerification {

    private const val MIMETYPE_AUDIO_ALAC = "audio/alac"

    fun checkNativeSupport(): AlacSupportReport {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.Q) {
            return AlacSupportReport(
                isSupportedNatively = false,
                codecName = null,
                isHardwareAccelerated = false,
                isSoftwareOnly = false,
                details = "Android API level ${Build.VERSION.SDK_INT} < 29 (ALAC native decoder introduced in Android 10)"
            )
        }

        return try {
            val codecList = MediaCodecList(MediaCodecList.REGULAR_CODECS)
            var foundCodecName: String? = null
            var isHw = false
            var isSw = false

            for (info in codecList.codecInfos) {
                if (info.isEncoder) continue

                for (type in info.supportedTypes) {
                    if (type.equals(MIMETYPE_AUDIO_ALAC, ignoreCase = true)) {
                        foundCodecName = info.name
                        isHw = info.isHardwareAccelerated
                        isSw = info.isSoftwareOnly
                        break
                    }
                }
                if (foundCodecName != null) break
            }

            if (foundCodecName != null) {
                AlacSupportReport(
                    isSupportedNatively = true,
                    codecName = foundCodecName,
                    isHardwareAccelerated = isHw,
                    isSoftwareOnly = isSw,
                    details = "Native ALAC decoder '$foundCodecName' found (Hardware: $isHw, Software: $isSw)"
                )
            } else {
                AlacSupportReport(
                    isSupportedNatively = false,
                    codecName = null,
                    isHardwareAccelerated = false,
                    isSoftwareOnly = false,
                    details = "No native decoder registered for $MIMETYPE_AUDIO_ALAC in MediaCodecList"
                )
            }
        } catch (e: Exception) {
            AlacSupportReport(
                isSupportedNatively = false,
                codecName = null,
                isHardwareAccelerated = false,
                isSoftwareOnly = false,
                details = "Inspection failed with error: ${e.message}"
            )
        }
    }
}
