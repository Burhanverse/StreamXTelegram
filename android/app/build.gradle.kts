plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "com.music.streamx.extensions.telegram"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.music.streamx.extensions.telegram"
        minSdk = 26
        targetSdk = 35
        versionCode = 2
        versionName = "1.1"

        ndk {
            abiFilters += setOf("arm64-v8a")
        }

        resourceConfigurations += setOf("en")

        val baseHashes = mutableListOf(
            "\"36e144d897f31ccdf6a9b8321aa2b3828837fe3d263281957cc3b7718066d2cc\"",
            "\"f0d96c1f8f886ba0c4049fdfeaddf3f945e06ac5c623f734f7a88d28edff10f2\""
        )
        val extraCert = providers.gradleProperty("RELEASE_CERT_SHA256").orNull
            ?: System.getenv("RELEASE_CERT_SHA256")
        if (!extraCert.isNullOrBlank()) {
            val sanitized = extraCert.replace(":", "").lowercase().trim()
            if (sanitized.isNotEmpty()) {
                baseHashes.add("\"$sanitized\"")
            }
        }
        val hashesLiteral = "new String[]{${baseHashes.joinToString(", ")}}"

        buildConfigField("String", "PROVIDER_AUTHORITY", "\"com.music.streamx.extensions.telegram.source\"")
        buildConfigField("String[]", "ALLOWED_CALLER_PACKAGES", "new String[]{\"com.music.streamx\", \"com.dev.streamx\", \"com.music.streamx.benchmark\", \"com.dev.streamx.benchmark\"}")
        buildConfigField("String[]", "ALLOWED_CERTIFICATE_HASHES", hashesLiteral)
    }

    signingConfigs {
        create("release") {
            val storeFilePath = providers.gradleProperty("RELEASE_STORE_FILE").orNull
                ?: System.getenv("RELEASE_STORE_FILE")
                ?: providers.gradleProperty("KEYSTORE_PATH").orNull
                ?: System.getenv("KEYSTORE_PATH")
            val storePass = providers.gradleProperty("RELEASE_STORE_PASSWORD").orNull
                ?: System.getenv("RELEASE_STORE_PASSWORD")
                ?: providers.gradleProperty("KEYSTORE_PASSWORD").orNull
                ?: System.getenv("KEYSTORE_PASSWORD")
            val keyAl = providers.gradleProperty("RELEASE_KEY_ALIAS").orNull
                ?: System.getenv("RELEASE_KEY_ALIAS")
                ?: providers.gradleProperty("KEY_ALIAS").orNull
                ?: System.getenv("KEY_ALIAS")
            val keyPass = providers.gradleProperty("RELEASE_KEY_PASSWORD").orNull
                ?: System.getenv("RELEASE_KEY_PASSWORD")
                ?: providers.gradleProperty("KEY_PASSWORD").orNull
                ?: System.getenv("KEY_PASSWORD")

            if (!storeFilePath.isNullOrBlank() && file(storeFilePath).exists()) {
                storeFile = file(storeFilePath)
                storePassword = storePass ?: ""
                keyAlias = keyAl ?: ""
                keyPassword = keyPass ?: storePass ?: ""
            } else {
                val debugKey = file("${System.getProperty("user.home")}/.android/debug.keystore")
                if (debugKey.exists()) {
                    storeFile = debugKey
                    storePassword = "android"
                    keyAlias = "androiddebugkey"
                    keyPassword = "android"
                }
            }
        }
    }

    buildTypes {
        release {
            isDebuggable = false
            isMinifyEnabled = true
            isShrinkResources = true
            signingConfig = signingConfigs.getByName("release")
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            ndk {
                debugSymbolLevel = "NONE"
            }
        }
        debug {
            signingConfig = signingConfigs.getByName("release")
        }
    }

    packaging {
        jniLibs {
            useLegacyPackaging = true
            keepDebugSymbols.clear()
        }
        resources {
            excludes += setOf(
                "META-INF/*.version",
                "META-INF/DEPENDENCIES",
                "META-INF/LICENSE*",
                "META-INF/NOTICE*",
                "META-INF/*.txt"
            )
        }
    }

    buildFeatures {
        buildConfig = true
        viewBinding = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_21
        targetCompatibility = JavaVersion.VERSION_21
    }
    kotlinOptions {
        jvmTarget = "21"
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    implementation(fileTree(mapOf("dir" to "libs", "include" to listOf("*.aar"))))

    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")

    // Media3 ExoPlayer
    val media3Version = "1.5.1"
    implementation("androidx.media3:media3-exoplayer:$media3Version")
    implementation("androidx.media3:media3-common:$media3Version")

    // Coroutines
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.1")

    // Unit testing
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20240303")
}
