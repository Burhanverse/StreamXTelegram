package com.music.streamx.extensions.telegram

import org.junit.Assert.assertNotNull
import org.junit.Test

class AlacVerificationTest {

    @Test
    fun testAlacVerificationReports() {
        val report = AlacVerification.checkNativeSupport()
        assertNotNull("Report should not be null", report)
        assertNotNull("Report details should not be null", report.details)
        println("ALAC Codec Report: native=${report.isSupportedNatively}, codec=${report.codecName}, details=${report.details}")
    }
}
