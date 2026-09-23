// Top-level build file. Plugins are declared here (not applied) so every module
// resolves them at one version.
//
// There is deliberately no org.jetbrains.kotlin.android here: AGP 9 compiles
// Kotlin itself ("built-in Kotlin"), and applying that plugin alongside it is a
// hard error. The two Kotlin compiler plugins below are still needed, and their
// version is what pins the Kotlin compiler this build uses.
plugins {
    alias(libs.plugins.android.application) apply false
    alias(libs.plugins.kotlin.compose) apply false
    alias(libs.plugins.kotlin.serialization) apply false
}
