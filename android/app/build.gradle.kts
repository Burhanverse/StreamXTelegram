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
        versionCode = 1
        versionName = "1.0.0"

        ndk {
            abiFilters += setOf("arm64-v8a")
        }

        resourceConfigurations += setOf("en")

        buildConfigField("String", "PROVIDER_AUTHORITY", "\"com.music.streamx.extensions.telegram.source\"")
        buildConfigField("String[]", "ALLOWED_CALLER_PACKAGES", "new String[]{\"com.music.streamx\", \"com.dev.streamx\", \"com.music.streamx.benchmark\", \"com.dev.streamx.benchmark\"}")
        buildConfigField("String[]", "ALLOWED_CERTIFICATE_HASHES", "new String[]{\"36e144d897f31ccdf6a9b8321aa2b3828837fe3d263281957cc3b7718066d2cc\", \"f0d96c1f8f886ba0c4049fdfeaddf3f945e06ac5c623f734f7a88d28edff10f2\"}")
    }

    signingConfigs {
        create("release") {
            val debugKey = file("${System.getProperty("user.home")}/.android/debug.keystore")
            if (debugKey.exists()) {
                storeFile = debugKey
                storePassword = "android"
                keyAlias = "androiddebugkey"
                keyPassword = "android"
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            signingConfig = signingConfigs.getByName("release")
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
        }
        debug {
            signingConfig = signingConfigs.getByName("release")
        }
    }

    packaging {
        jniLibs {
            useLegacyPackaging = true
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
    implementation(fileTree(mapOf("dir" to "libs", "include" to listOf("*.aar", "*.jar"))))

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
