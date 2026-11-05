package com.music.streamx.extensions.telegram

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ArtworkPathTest {

    @Test
    fun testArtworkPathAgreement() {
        // Shared directory constant must be "artwork" across both Go engine and Kotlin ContentProvider
        val providerDir = StreamXProvider.ARTWORK_DIR_NAME
        assertEquals("StreamXProvider must use 'artwork' directory", "artwork", providerDir)

        val helperDir = StreamXProvider.getArtworkDirName()
        assertEquals("getArtworkDirName must agree on 'artwork'", "artwork", helperDir)
    }

    @Test
    fun testProviderGetTypeForArtAndTrack() {
        val provider = StreamXProvider()

        assertEquals("image/jpeg", provider.getTypeForSegments(listOf("art", "track123")))
        assertEquals("audio/mpeg", provider.getTypeForSegments(listOf("track", "track123")))
        assertNull(provider.getTypeForSegments(listOf("unknown", "123")))
        assertNull(provider.getTypeForSegments(emptyList()))
    }
}


