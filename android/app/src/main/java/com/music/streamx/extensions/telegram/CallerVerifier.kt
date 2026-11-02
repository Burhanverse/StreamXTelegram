package com.music.streamx.extensions.telegram

import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.Process
import java.security.MessageDigest

/**
 * SignatureChecker abstracts Android PackageManager signature queries
 * for caller identity and cryptographic certificate verification.
 */
interface SignatureChecker {
    fun getPackagesForUid(uid: Int): Array<String>?
    fun hasSigningCertificate(packageName: String, certBytes: ByteArray): Boolean
    fun getSignaturesSha256(packageName: String): List<String>?
}

/**
 * DefaultSignatureChecker implements SignatureChecker using the Android system PackageManager.
 */
class DefaultSignatureChecker(private val context: Context) : SignatureChecker {

    override fun getPackagesForUid(uid: Int): Array<String>? =
        context.packageManager.getPackagesForUid(uid)

    override fun hasSigningCertificate(packageName: String, certBytes: ByteArray): Boolean {
        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) {
            context.packageManager.hasSigningCertificate(
                packageName,
                certBytes,
                PackageManager.CERT_INPUT_SHA256
            )
        } else {
            val certHex = certBytes.joinToString("") { "%02x".format(it) }
            val sigs = getSignaturesSha256(packageName) ?: return false
            sigs.any { it.equals(certHex, ignoreCase = true) }
        }
    }

    override fun getSignaturesSha256(packageName: String): List<String>? {
        return runCatching {
            @Suppress("DEPRECATION")
            val info = context.packageManager.getPackageInfo(packageName, PackageManager.GET_SIGNATURES)
            val signatures = info.signatures ?: return null
            signatures.map { sig ->
                val digest = MessageDigest.getInstance("SHA-256").digest(sig.toByteArray())
                digest.joinToString("") { "%02x".format(it) }
            }
        }.getOrNull()
    }
}

/**
 * CallerVerifier provides cryptographic caller verification for ContentProviders.
 */
object CallerVerifier {

    fun verifyCaller(
        context: Context,
        callingUid: Int,
        callingPackage: String?,
        allowedPackages: Array<String> = BuildConfig.ALLOWED_CALLER_PACKAGES,
        allowedCertHashes: Array<String> = BuildConfig.ALLOWED_CERTIFICATE_HASHES
    ) {
        verifyCaller(
            callingUid = callingUid,
            callingPackage = callingPackage,
            signatureChecker = DefaultSignatureChecker(context),
            allowedPackages = allowedPackages,
            allowedCertHashes = allowedCertHashes
        )
    }

    fun verifyCaller(
        callingUid: Int,
        callingPackage: String?,
        signatureChecker: SignatureChecker,
        allowedPackages: Array<String> = BuildConfig.ALLOWED_CALLER_PACKAGES,
        allowedCertHashes: Array<String> = BuildConfig.ALLOWED_CERTIFICATE_HASHES
    ) {
        // Self-invocation within the same process / UID is always trusted
        if (callingUid == Process.myUid()) {
            return
        }

        // Calling package must be present
        if (callingPackage.isNullOrBlank()) {
            throw SecurityException("Caller verification failed: missing calling package for UID $callingUid")
        }

        // 1. Verify caller package is in the allowlist
        if (!allowedPackages.contains(callingPackage)) {
            throw SecurityException("Caller package '$callingPackage' is not in allowed list")
        }

        // 2. Fail closed if allowed certificate hashes list is empty
        if (allowedCertHashes.isEmpty()) {
            throw SecurityException("Caller verification failed: allowed certificate hashes list is empty (fail-closed)")
        }

        // 3. Verify UID owns the claimed callingPackage
        val packagesForUid = signatureChecker.getPackagesForUid(callingUid)
        if (packagesForUid == null || !packagesForUid.contains(callingPackage)) {
            throw SecurityException("UID $callingUid does not own claimed package '$callingPackage'")
        }

        // 4. Verify caller's signing certificate against allowed hashes
        val certMatched = isCertificateAllowed(signatureChecker, callingPackage, allowedCertHashes)
        if (!certMatched) {
            throw SecurityException("Caller '$callingPackage' signing certificate does not match allowed hashes")
        }
    }

    fun isCertificateAllowed(
        checker: SignatureChecker,
        packageName: String,
        allowedHashes: Array<String>
    ): Boolean {
        if (allowedHashes.isEmpty()) return false

        val normalizedAllowed = allowedHashes.map { normalizeHex(it) }.toSet()

        for (hashHex in normalizedAllowed) {
            val certBytes = hexToByteArray(hashHex)
            if (checker.hasSigningCertificate(packageName, certBytes)) {
                return true
            }
        }

        // Fallback for signatures
        val sigs = checker.getSignaturesSha256(packageName) ?: return false
        return sigs.any { normalizedAllowed.contains(normalizeHex(it)) }
    }

    internal fun normalizeHex(hex: String): String =
        hex.replace(":", "").lowercase()

    internal fun hexToByteArray(hex: String): ByteArray {
        val clean = normalizeHex(hex)
        val len = clean.length
        val data = ByteArray(len / 2)
        for (i in 0 until len step 2) {
            data[i / 2] = ((Character.digit(clean[i], 16) shl 4) +
                    Character.digit(clean[i + 1], 16)).toByte()
        }
        return data
    }
}
