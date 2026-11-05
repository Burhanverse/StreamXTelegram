# Optimization and size reduction
-optimizationpasses 5
-allowaccessmodification

# Gomobile runtime and Go wrappers
-keep class go.** { *; }
-keep class engine.** { *; }

# EventCallback interface and all implementations (including anonymous classes)
-keep class engine.EventCallback { *; }
-keep class * implements engine.EventCallback { *; }

# Extension IPC / ContentProvider / Service / UI / Manifest components
-keep class com.music.streamx.extensions.telegram.** { *; }
-keep class * extends android.content.ContentProvider { <init>(...); }
-keep class * extends android.app.Service { <init>(...); }
-keep class * extends android.app.Activity { <init>(...); }
-keep class * extends android.content.BroadcastReceiver { <init>(...); }

# JNI native methods
-keepclasseswithmembers class * {
    native <methods>;
}

# Strip Android verbose and debug logging in release builds (Log.v / Log.d)
# Keeps Log.i, Log.w, Log.e, our internal LogBuffer, and diagnostics intact
-assumenosideeffects class android.util.Log {
    public static boolean isLoggable(java.lang.String, int);
    public static int v(...);
    public static int d(...);
}

# Media3 & ExoPlayer (Consumer proguard rules are already embedded in AAR)
-dontwarn androidx.media3.**

# Coroutines (Consumer proguard rules are already embedded in AAR)
-dontwarn kotlinx.coroutines.**
