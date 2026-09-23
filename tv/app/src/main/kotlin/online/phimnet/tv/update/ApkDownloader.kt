package online.phimnet.tv.update

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import okhttp3.OkHttpClient
import okhttp3.Request
import online.phimnet.tv.api.AppRelease
import online.phimnet.tv.api.await
import java.io.File
import java.io.IOException
import java.security.MessageDigest

/** The bytes did not match what the manifest promised. */
class IntegrityException(message: String) : IOException(message)

/**
 * Downloads a release's APK and verifies it against the manifest — SHA-256 and
 * exact size — before handing it back. It never returns an unverified file: the
 * bytes land in a temporary file, which is renamed into place only once both
 * checks pass and is deleted otherwise.
 */
class ApkDownloader(private val http: OkHttpClient, private val userAgent: String) {

    suspend fun download(release: AppRelease, dest: File, onProgress: (Float) -> Unit = {}): File =
        withContext(Dispatchers.IO) {
            dest.parentFile?.mkdirs()
            val partial = File(dest.path + ".part")
            try {
                val request = Request.Builder()
                    .url(release.downloadUrl)
                    .header("User-Agent", userAgent)
                    .build()
                http.newCall(request).await().use { response ->
                    if (!response.isSuccessful) throw IOException("HTTP ${response.code}")
                    val digest = MessageDigest.getInstance("SHA-256")
                    var received = 0L
                    response.body.byteStream().use { input ->
                        partial.outputStream().use { output ->
                            val buffer = ByteArray(64 shl 10)
                            while (true) {
                                ensureActive()
                                val n = input.read(buffer)
                                if (n < 0) break
                                received += n
                                // Stop the moment it overruns what the manifest
                                // promised, rather than filling the disk first.
                                if (received > release.size) {
                                    throw IntegrityException("more than the ${release.size} bytes promised")
                                }
                                digest.update(buffer, 0, n)
                                output.write(buffer, 0, n)
                                onProgress(received.toFloat() / release.size)
                            }
                        }
                    }
                    if (received != release.size) {
                        throw IntegrityException("got $received bytes, expected ${release.size}")
                    }
                    val sha = digest.digest().joinToString("") { "%02x".format(it) }
                    if (sha != release.sha256) throw IntegrityException("SHA-256 $sha, expected ${release.sha256}")
                }
                if (dest.exists()) dest.delete()
                if (!partial.renameTo(dest)) throw IOException("could not move the download into place")
                dest
            } finally {
                partial.delete() // a no-op after a successful rename
            }
        }
}
