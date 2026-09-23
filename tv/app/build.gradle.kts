plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
    alias(libs.plugins.kotlin.serialization)
}

android {
    namespace = "online.phimnet.tv"
    // Compile against Android 17 because current Compose, Navigation and Coil
    // refuse anything older (their AAR metadata demands it). This only widens the
    // APIs visible at compile time; targetSdk below is what opts into new runtime
    // behaviour, and it stays on 16.
    compileSdk = 37

    defaultConfig {
        applicationId = "online.phimnet.tv"
        // Android 7. Cheap Android TV boxes linger on old releases for years, so
        // this is as low as the dependencies allow — Navigation 2.10 is the floor.
        minSdk = 24
        targetSdk = 36
        // CI stamps these from the release tag (tv-v1.2.3 → "1.2.3" / 1002003; see
        // .github/workflows/android.yml). Android refuses an "update" whose
        // versionCode is not higher than the installed one, so a published build
        // must never reuse a code. Local builds get 1, below any release.
        versionCode = System.getenv("PHIMNET_TV_VERSION_CODE")?.toInt() ?: 1
        versionName = System.getenv("PHIMNET_TV_VERSION_NAME") ?: "0.0.0-dev"

        // The viewer this build talks to on first launch. phimtor2 is
        // self-hosted, so this is only a default: Settings can point the app at
        // any viewer, and that choice is what the app actually uses.
        buildConfigField("String", "DEFAULT_SERVER_URL", "\"https://phimnet.online\"")
        buildConfigField("boolean", "ALLOW_CLEARTEXT", "false")
    }

    buildTypes {
        debug {
            applicationIdSuffix = ".debug"
            // 10.0.2.2 is the emulator's alias for the host machine's loopback,
            // which is where `go run .` in viewer/ listens during development.
            buildConfigField("String", "DEFAULT_SERVER_URL", "\"http://10.0.2.2:8082\"")
            buildConfigField("boolean", "ALLOW_CLEARTEXT", "true")
        }
        release {
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
            // Signed from the environment when a keystore is provided (CI); an
            // unsigned release APK otherwise, which is what you want locally.
            val keystore = System.getenv("PHIMNET_TV_KEYSTORE")
            if (keystore != null) {
                signingConfig = signingConfigs.create("release") {
                    storeFile = file(keystore)
                    storePassword = System.getenv("PHIMNET_TV_KEYSTORE_PASSWORD")
                    keyAlias = System.getenv("PHIMNET_TV_KEY_ALIAS")
                    keyPassword = System.getenv("PHIMNET_TV_KEY_PASSWORD")
                }
            }
        }
    }

    // Release's R8 shrinking, with debug's signing and local-server defaults.
    // R8 breaking serialization or type-safe navigation only shows at RUNTIME,
    // when a screen loads — and a real release build cannot reach a local
    // `go run .` viewer (it is https-only). Install this to smoke-test the
    // minified app against a local backend before shipping.
    buildTypes.create("minified") {
        initWith(buildTypes.getByName("release"))
        applicationIdSuffix = ".minified"
        signingConfig = signingConfigs.getByName("debug")
        matchingFallbacks += "release"
        buildConfigField("String", "DEFAULT_SERVER_URL", "\"http://10.0.2.2:8082\"")
        buildConfigField("boolean", "ALLOW_CLEARTEXT", "true")
    }
    // Shares the debug variant's cleartext network config.
    sourceSets.getByName("minified").res.directories += "src/debug/res"

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }

    testOptions {
        // Unit tests run on the JVM; any android.* call a test reaches returns a
        // default rather than throwing. The logic under test is kept free of
        // Android types on purpose, so this is a safety net, not a crutch.
        unitTests.isReturnDefaultValues = true
    }
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.activity.compose)
    implementation(libs.androidx.navigation.compose)
    implementation(libs.androidx.lifecycle.runtime.compose)
    implementation(libs.androidx.lifecycle.viewmodel.compose)
    implementation(libs.androidx.datastore.preferences)

    implementation(platform(libs.compose.bom))
    implementation(libs.compose.foundation)
    implementation(libs.compose.ui)
    implementation(libs.compose.ui.tooling.preview)
    implementation(libs.tv.material)
    debugImplementation(libs.compose.ui.tooling)

    implementation(libs.media3.exoplayer)
    implementation(libs.media3.datasource.okhttp)
    implementation(libs.media3.ui)

    implementation(libs.kotlinx.serialization.json)
    implementation(libs.kotlinx.coroutines.android)
    implementation(libs.okhttp)
    implementation(libs.coil.compose)
    implementation(libs.coil.network.okhttp)
    implementation(libs.zxing.core)

    testImplementation(libs.junit)
    testImplementation(libs.kotlinx.coroutines.test)
    testImplementation(libs.okhttp.mockwebserver)
}
