package com.music.streamx.extensions.telegram

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Test

class CallerVerifierTest {

    private val validPkg = "com.music.streamx"
    private val validHash = "36e144d897f31ccdf6a9b8321aa2b3828837fe3d263281957cc3b7718066d2cc"
    private val wrongHash = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
    private val allowedPackages = arrayOf(validPkg, "com.dev.streamx")

    private class FakeSignatureChecker(
        private val matchingPackage: String,
        private val matchingHashHex: String
    ) : SignatureChecker {

        override fun getPackagesForUid(uid: Int): Array<String>? {
            return if (uid == 10200) arrayOf(matchingPackage) else null
        }

        override fun hasSigningCertificate(packageName: String, certBytes: ByteArray): Boolean {
            if (packageName != matchingPackage) return false
            val hex = certBytes.joinToString("") { "%02x".format(it) }
            return hex.equals(matchingHashHex.replace(":", ""), ignoreCase = true)
        }

        override fun getSignaturesSha256(packageName: String): List<String>? {
            return if (packageName == matchingPackage) listOf(matchingHashHex.replace(":", "").lowercase()) else null
        }
    }

    @Test
    fun testAllowedPackageAndCertSucceeds() {
        val fakeChecker = FakeSignatureChecker(validPkg, validHash)

        // Should complete without throwing SecurityException
        CallerVerifier.verifyCaller(
            callingUid = 10200,
            callingPackage = validPkg,
            signatureChecker = fakeChecker,
            allowedPackages = allowedPackages,
            allowedCertHashes = arrayOf(validHash)
        )
    }

    @Test
    fun testWrongPackageFails() {
        val fakeChecker = FakeSignatureChecker(validPkg, validHash)

        try {
            CallerVerifier.verifyCaller(
                callingUid = 10200,
                callingPackage = "com.untrusted.host",
                signatureChecker = fakeChecker,
                allowedPackages = allowedPackages,
                allowedCertHashes = arrayOf(validHash)
            )
            fail("Expected SecurityException for untrusted package")
        } catch (e: SecurityException) {
            assertTrue(e.message?.contains("not in allowed list") == true)
        }
    }

    @Test
    fun testRightPackageWithWrongCertFails() {
        val fakeChecker = FakeSignatureChecker(validPkg, validHash) // system has validHash

        try {
            CallerVerifier.verifyCaller(
                callingUid = 10200,
                callingPackage = validPkg,
                signatureChecker = fakeChecker,
                allowedPackages = allowedPackages,
                allowedCertHashes = arrayOf(wrongHash) // allowlist only allows wrongHash
            )
            fail("Expected SecurityException for certificate mismatch")
        } catch (e: SecurityException) {
            assertTrue(e.message?.contains("signing certificate does not match") == true)
        }
    }

    @Test
    fun testEmptyAllowlistFailsClosed() {
        val fakeChecker = FakeSignatureChecker(validPkg, validHash)

        try {
            CallerVerifier.verifyCaller(
                callingUid = 10200,
                callingPackage = validPkg,
                signatureChecker = fakeChecker,
                allowedPackages = allowedPackages,
                allowedCertHashes = emptyArray() // Fail closed!
            )
            fail("Expected SecurityException when allowlist is empty")
        } catch (e: SecurityException) {
            assertTrue(e.message?.contains("fail-closed") == true)
        }
    }

    @Test
    fun testUidMismatchFails() {
        val fakeChecker = FakeSignatureChecker(validPkg, validHash)

        try {
            // UID 99999 does not match UID 10200 which owns validPkg
            CallerVerifier.verifyCaller(
                callingUid = 99999,
                callingPackage = validPkg,
                signatureChecker = fakeChecker,
                allowedPackages = allowedPackages,
                allowedCertHashes = arrayOf(validHash)
            )
            fail("Expected SecurityException when UID does not own claimed package")
        } catch (e: SecurityException) {
            assertTrue(e.message?.contains("does not own claimed package") == true)
        }
    }

    @Test
    fun testHexConversion() {
        val hex = "36e144d897f3"
        val bytes = CallerVerifier.hexToByteArray(hex)
        assertEquals(6, bytes.size)
        assertEquals(0x36.toByte(), bytes[0])
        assertEquals(0xe1.toByte(), bytes[1])
        assertEquals("36e144d8", CallerVerifier.normalizeHex("36:E1:44:D8"))
    }
}
