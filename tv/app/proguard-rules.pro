# kotlinx.serialization, OkHttp, Media3, Coil and Navigation all ship their own
# consumer R8 rules, so nothing is needed here for them.
#
# The wire DTOs in online.phimnet.tv.api are @Serializable and reached only
# through their generated serializers, which the kotlinx rules already keep.
