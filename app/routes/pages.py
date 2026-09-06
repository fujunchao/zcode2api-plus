"""頁面路由：根路徑重導、管理後台 SPA（frontend/dist）與版本 meta。"""

from __future__ import annotations

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse, RedirectResponse

from .. import settings

router = APIRouter()


def _index() -> FileResponse:
    """回傳 SPA 入口頁；建置產物缺失時明確報 404，提示需先建置前端。"""
    index = settings.FRONTEND_DIST / "index.html"
    if not index.exists():
        raise HTTPException(404, "管理後台尚未建置：缺少 frontend/dist，請先執行 npm run build")
    return FileResponse(index, headers={"Cache-Control": "no-store"})


@router.get("/", include_in_schema=False)
async def root():
    return RedirectResponse("/admin")


@router.get("/admin", include_in_schema=False)
async def admin_root():
    return RedirectResponse("/admin/dashboard")


@router.get("/admin/{path:path}", include_in_schema=False)
async def admin_spa(path: str):
    # SPA catch-all：登入頁與所有內部路由一律回落 index.html，重新整理不落 404。
    # 本路由必須在 admin_api（/admin/api）之後註冊，見 main.py 的 include 順序。
    return _index()


@router.get("/meta", include_in_schema=False)
async def meta():
    return {"version": settings.APP_VERSION}
