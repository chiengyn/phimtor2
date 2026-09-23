package online.phimnet.tv

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.CompositionLocalProvider
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.tv.material3.MaterialTheme
import androidx.tv.material3.Surface
import androidx.tv.material3.SurfaceDefaults
import online.phimnet.tv.ui.AppNav
import online.phimnet.tv.ui.theme.PhimnetTheme

class MainActivity : ComponentActivity() {

    override fun onResume() {
        super.onResume()
        (application as PhimnetApp).graph.updater.onAppResumed()
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val graph = (application as PhimnetApp).graph
        setContent {
            PhimnetTheme {
                CompositionLocalProvider(LocalGraph provides graph) {
                    // A Surface at the root is what gives every Text its colour:
                    // tv-material's Text inherits LocalContentColor, which is
                    // otherwise unset — black text on a black screen.
                    Surface(
                        modifier = Modifier.fillMaxSize(),
                        colors = SurfaceDefaults.colors(
                            containerColor = MaterialTheme.colorScheme.background,
                            contentColor = MaterialTheme.colorScheme.onBackground,
                        ),
                    ) {
                        val prefs by graph.settings.prefs.collectAsStateWithLifecycle()
                        val loaded = prefs != null
                        // Once per launch, silently, and only once settings are
                        // read: before that, requests would go to the DEFAULT
                        // server, not the one this TV is configured for. The
                        // result waits on the Home screen.
                        LaunchedEffect(loaded) { if (loaded) graph.updater.check() }
                        // Wait for the first settings read, so the very first
                        // request already goes to the right server with the right token.
                        if (!loaded) Box(Modifier.fillMaxSize()) else AppNav()
                    }
                }
            }
        }
    }
}
