import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
    alias(libs.plugins.kotlin.compose)
}

// Firebase project values for push notifications, from the Firebase
// console's Android app settings. Without them the app works but only
// notifies while open. See android/README.md.
val firebase = Properties().apply {
    rootDir.resolve("firebase.properties").takeIf { it.exists() }?.inputStream()?.use { load(it) }
}
fun firebaseValue(key: String) = "\"" + firebase.getProperty(key, "") + "\""

android {
    namespace = "us.peerb.app"
    compileSdk = 37

    defaultConfig {
        applicationId = "us.peerb.app"
        minSdk = 26
        targetSdk = 37
        versionCode = 1
        versionName = "0.2.0"

        buildConfigField("String", "FIREBASE_APP_ID", firebaseValue("appId"))
        buildConfigField("String", "FIREBASE_API_KEY", firebaseValue("apiKey"))
        buildConfigField("String", "FIREBASE_PROJECT_ID", firebaseValue("projectId"))
        buildConfigField("String", "FIREBASE_SENDER_ID", firebaseValue("senderId"))
    }

    buildTypes {
        release {
            isMinifyEnabled = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            // Sideloaded builds: sign with the debug key until there's a release key.
            signingConfig = signingConfigs.getByName("debug")
        }
    }

    buildFeatures {
        compose = true
        buildConfig = true
    }
}

// A full JDK even when the one running Gradle is only a runtime.
kotlin {
    jvmToolchain(25)
}

// The Go core: ../mobile bound with gomobile into app/libs/peerbus.aar.
val buildCore = tasks.register<Exec>("buildCore") {
    description = "Builds the Go core with gomobile"
    workingDir = rootDir
    commandLine("./build-core.sh")
    inputs.dir(rootDir.resolve("../mobile"))
    inputs.dir(rootDir.resolve("../internal"))
    inputs.file(rootDir.resolve("../go.mod"))
    inputs.file(rootDir.resolve("build-core.sh"))
    outputs.file(file("libs/peerbus.aar"))
    // gomobile runs javac itself; give it the toolchain's JDK.
    val jdk = javaToolchains.compilerFor { languageVersion = JavaLanguageVersion.of(25) }
    doFirst {
        val home = jdk.get().metadata.installationPath.asFile
        environment("JAVA_HOME", home.path)
        environment("PATH", home.resolve("bin").path + File.pathSeparator + System.getenv("PATH"))
    }
}
tasks.named("preBuild") { dependsOn(buildCore) }

dependencies {
    implementation(files("libs/peerbus.aar"))

    implementation(platform(libs.compose.bom))
    implementation(libs.compose.ui)
    implementation(libs.compose.foundation)
    implementation(libs.compose.material3)
    implementation(libs.compose.tooling.preview)
    debugImplementation(libs.compose.tooling)
    implementation(libs.activity.compose)
    implementation(libs.lifecycle.runtime.compose)
    implementation(libs.lifecycle.process)
    implementation(libs.core.ktx)
    implementation(libs.coroutines.android)

    implementation(libs.camera.camera2)
    implementation(libs.camera.lifecycle)
    implementation(libs.camera.compose)
    implementation(libs.camera.mlkit.vision)
    implementation(libs.mlkit.barcode)

    implementation(libs.firebase.messaging)
}
