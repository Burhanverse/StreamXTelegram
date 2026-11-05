# Optimization and size reduction
-optimizationpasses 5
-allowaccessmodification

# Gomobile runtime and Go wrappers
-keep class go.** { *; }
-keep class engine.** { *; }

# Extension IPC / ContentProvider / Service / UI
-keep class com.music.streamx.extensions.telegram.** { *; }
-keep class * extends android.content.ContentProvider { <init>(...); }
-keep class * extends android.app.Service { <init>(...); }
-keep class * extends android.app.Activity { <init>(...); }

# Media3 & ExoPlayer (Consumer proguard rules are already embedded in AAR)
-dontwarn androidx.media3.**

# Coroutines (Consumer proguard rules are already embedded in AAR)
-dontwarn kotlinx.coroutines.**
